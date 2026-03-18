package logic

import (
	"context"
	"fmt"

	"github.com/q191201771/lal/pkg/base"
)

// FfmpegJpegPool 用于限制截图解码并发量。
//
// 设计目标：
// - 控制并发上限，防止截图流量拖垮主进程
//
// 注意：
// - 为稳定性考虑，这里不复用“常驻 ffmpeg + stdin 连续喂流”的模式（容易出现输入边界不确定导致阻塞或读写错位）。
// - 采用“并发受控的单次解码”：每次任务独立拉起 ffmpeg，完成即退出；通过信号量限制并发。
type FfmpegJpegPool struct {
	sem chan struct{}
}

func NewFfmpegJpegPool(poolSize int) *FfmpegJpegPool {
	if poolSize < 0 {
		poolSize = 0
	}
	if poolSize == 0 {
		return nil
	}
	return &FfmpegJpegPool{sem: make(chan struct{}, poolSize)}
}

func (p *FfmpegJpegPool) Decode(ctx context.Context, frame *SnapshotFrame) ([]byte, error) {
	if frame == nil || len(frame.Data) == 0 {
		return nil, fmt.Errorf("empty snapshot frame")
	}
	if payloadTypeToCodec(frame.PayloadType) == "" {
		return nil, fmt.Errorf("unsupported payload type for snapshot: %d", frame.PayloadType)
	}

	// poolSize=0 时退化为单次拉起 ffmpeg（兼容，但不推荐）
	if p == nil {
		return convertAnnexbToJPEG(frame)
	}

	select {
	case p.sem <- struct{}{}:
		defer func() { <-p.sem }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return convertAnnexbToJPEG(frame)
}

func payloadTypeToCodec(pt base.AvPacketPt) string {
	switch pt {
	case base.AvPacketPtAvc:
		return "h264"
	case base.AvPacketPtHevc:
		return "hevc"
	default:
		return ""
	}
}
