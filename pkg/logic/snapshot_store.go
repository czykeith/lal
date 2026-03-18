package logic

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/q191201771/lal/pkg/base"
)

// SnapshotFrame 表示一帧可用于截图的关键帧（AnnexB H264/H265）。
// Data 通常为带起始码的 SPS/PPS/VPS + IDR。
type SnapshotFrame struct {
	PayloadType base.AvPacketPt
	Timestamp   int64
	Data        []byte
}

var (
	ErrSnapshotNotFound              = errors.New("snapshot not found")
	ErrSnapshotDecodeInFlightTimeout = errors.New("snapshot decode in flight timeout")
	ErrSnapshotBusy                  = errors.New("snapshot busy")
)

type snapshotItem struct {
	frame *SnapshotFrame

	mu sync.Mutex
	// jpg 缓存对应的 frame.Timestamp
	jpgTs int64
	jpg   []byte

	// 并发去抖：同一路同时只允许一个解码任务执行，其它请求等待结果复用。
	inFlight bool
	waitCh   chan struct{}

	// 记录最近一次解码错误，避免错误风暴时每个请求都重复拉起 ffmpeg
	lastErr   error
	lastErrAt time.Time
}

// SnapshotStore 以 streamName 为维度缓存每路流最新的一帧关键帧。
type SnapshotStore struct {
	mu sync.RWMutex
	m  map[string]*snapshotItem

	decoder *FfmpegJpegPool
}

func NewSnapshotStore(ffmpegPoolSize int) *SnapshotStore {
	return &SnapshotStore{
		m:       make(map[string]*snapshotItem),
		decoder: NewFfmpegJpegPool(ffmpegPoolSize),
	}
}

// Update 覆盖指定 streamName 的最新关键帧。
// 约定：调用方只在确认是「带 SPS/PPS/VPS 的关键帧」时才调用。
func (s *SnapshotStore) Update(streamName string, pkt base.AvPacket) {
	if streamName == "" || !pkt.IsVideo() || len(pkt.Payload) == 0 {
		return
	}

	// 为保证单帧可被独立解码，必须确保 AnnexB 数据中包含必要的参数集。
	// 对 H264 来说，至少要同时带 SPS 和 PPS；否则 ffmpeg 会报 non-existing PPS / no frame。
	if pkt.PayloadType == base.AvPacketPtAvc && !hasH264SpsPps(pkt.Payload) {
		// 等待下一次带 SPS/PPS 的关键帧再更新缓存
		return
	}

	data := make([]byte, len(pkt.Payload))
	copy(data, pkt.Payload)

	s.mu.Lock()
	item, ok := s.m[streamName]
	if !ok || item == nil {
		item = &snapshotItem{}
		s.m[streamName] = item
	}
	item.frame = &SnapshotFrame{
		PayloadType: pkt.PayloadType,
		Timestamp:   pkt.Timestamp,
		Data:        data,
	}
	// 帧更新后清空 JPEG 缓存；若有 inFlight 解码，完成后会写入新的缓存或被下一次覆盖。
	item.mu.Lock()
	item.jpgTs = 0
	item.jpg = nil
	item.lastErr = nil
	item.lastErrAt = time.Time{}
	item.mu.Unlock()
	s.mu.Unlock()
}

// hasH264SpsPps 粗略检查 AnnexB H264 数据中是否同时包含 SPS(7) 和 PPS(8) NAL 单元。
// 只为截图场景做健壮性过滤，不追求完整 H264 规范覆盖。
func hasH264SpsPps(b []byte) bool {
	const (
		nalSps = 7
		nalPps = 8
	)
	hasSps, hasPps := false, false
	n := len(b)
	for i := 0; i+4 < n; i++ {
		// 查找 0x00 00 00 01 起始码
		if b[i] == 0x00 && b[i+1] == 0x00 && b[i+2] == 0x00 && b[i+3] == 0x01 {
			nalHeader := b[i+4]
			nalType := nalHeader & 0x1F
			if nalType == nalSps {
				hasSps = true
			} else if nalType == nalPps {
				hasPps = true
			}
			if hasSps && hasPps {
				return true
			}
		}
	}
	return hasSps && hasPps
}

func (s *SnapshotStore) Get(streamName string) (*SnapshotFrame, bool) {
	if streamName == "" {
		return nil, false
	}
	s.mu.RLock()
	item, ok := s.m[streamName]
	s.mu.RUnlock()
	if !ok || item == nil || item.frame == nil {
		return nil, false
	}
	return item.frame, true
}

// GetJpeg 返回指定 streamName 的最新关键帧对应的 JPEG。
// 该方法会缓存 JPEG，避免每次请求都启动 ffmpeg；并对同一路做并发去抖。
func (s *SnapshotStore) GetJpeg(ctx context.Context, streamName string) ([]byte, error) {
	if streamName == "" {
		return nil, ErrSnapshotNotFound
	}

	s.mu.RLock()
	item, ok := s.m[streamName]
	s.mu.RUnlock()
	if !ok || item == nil || item.frame == nil || len(item.frame.Data) == 0 {
		return nil, ErrSnapshotNotFound
	}

	// 快速路径：命中缓存
	item.mu.Lock()
	if item.jpgTs == item.frame.Timestamp && len(item.jpg) > 0 {
		j := item.jpg
		item.mu.Unlock()
		return j, nil
	}

	// 若刚失败过，短时间内直接返回失败，避免错误风暴下反复拉起 ffmpeg
	if item.lastErr != nil && time.Since(item.lastErrAt) < time.Second {
		err := item.lastErr
		item.mu.Unlock()
		return nil, err
	}

	// 并发去抖：等待正在进行的解码
	if item.inFlight && item.waitCh != nil {
		ch := item.waitCh
		item.mu.Unlock()

		select {
		case <-ch:
			// 解码完成后再走一次缓存判断
			item.mu.Lock()
			if item.jpgTs == item.frame.Timestamp && len(item.jpg) > 0 {
				j := item.jpg
				item.mu.Unlock()
				return j, nil
			}
			err := item.lastErr
			item.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("%w: empty jpeg after in flight", ErrSnapshotDecodeInFlightTimeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// 开始一次新的解码
	item.inFlight = true
	item.waitCh = make(chan struct{})
	ch := item.waitCh
	frame := item.frame
	item.mu.Unlock()

	jpg, err := s.decoder.Decode(ctx, frame)

	item.mu.Lock()
	if err == nil && len(jpg) > 0 {
		item.jpgTs = frame.Timestamp
		item.jpg = jpg
		item.lastErr = nil
		item.lastErrAt = time.Time{}
	} else {
		if err == nil {
			err = fmt.Errorf("empty jpeg")
		}
		item.lastErr = err
		item.lastErrAt = time.Now()
		// 保持 jpg cache 为空，等待下一次请求或下一次关键帧更新
	}
	item.inFlight = false
	close(ch)
	item.waitCh = nil
	item.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return jpg, nil
}
