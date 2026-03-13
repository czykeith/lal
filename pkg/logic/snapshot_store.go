package logic

import (
	"sync"

	"github.com/q191201771/lal/pkg/base"
)

// SnapshotFrame 表示一帧可用于截图的关键帧（AnnexB H264/H265）。
// Data 通常为带起始码的 SPS/PPS/VPS + IDR。
type SnapshotFrame struct {
	PayloadType base.AvPacketPt
	Timestamp   int64
	Data        []byte
}

// SnapshotStore 以 streamName 为维度缓存每路流最新的一帧关键帧。
type SnapshotStore struct {
	mu sync.RWMutex
	m  map[string]*SnapshotFrame
}

func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{
		m: make(map[string]*SnapshotFrame),
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
	s.m[streamName] = &SnapshotFrame{
		PayloadType: pkt.PayloadType,
		Timestamp:   pkt.Timestamp,
		Data:        data,
	}
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
	f, ok := s.m[streamName]
	s.mu.RUnlock()
	return f, ok
}
