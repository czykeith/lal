package logic

import (
	"bytes"
	"fmt"
	"os/exec"
	"time"
)

// convertAnnexbToJPEG 使用外部 ffmpeg 将 AnnexB H264/H265 关键帧转成 JPEG。
// 为避免影响主流程性能，仅在 HTTP 截图请求时调用。
func convertAnnexbToJPEG(frame *SnapshotFrame) ([]byte, error) {
	if frame == nil || len(frame.Data) == 0 {
		return nil, fmt.Errorf("empty snapshot frame")
	}

	var codec string
	switch frame.PayloadType {
	case 0, 8, 14:
		return nil, fmt.Errorf("audio frame is not supported for snapshot")
	default:
		// 目前 AvPacketPtAvc / AvPacketPtHevc 已足够区分
		if frame.PayloadType == 96 {
			codec = "h264"
		} else if frame.PayloadType == 98 {
			codec = "hevc"
		} else {
			// 默认按 h264 处理，避免因 PayloadType 新增导致完全不可用
			codec = "h264"
		}
	}

	cmd := exec.Command(
		"ffmpeg",
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-threads", "1",
		"-f", codec, "-i", "pipe:0",
		"-an", "-sn",
		"-frames:v", "1",
		"-f", "mjpeg",
		"pipe:1",
	)
	cmd.Stdin = bytes.NewReader(frame.Data)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf

	done := make(chan error, 1)
	go func() {
		done <- cmd.Run()
	}()

	select {
	case err := <-done:
		if err != nil {
			// 将 ffmpeg 的 stderr 一并返回，便于排查具体失败原因。
			if errBuf.Len() > 0 {
				return nil, fmt.Errorf("ffmpeg run failed: %w, stderr: %s", err, errBuf.String())
			}
			return nil, fmt.Errorf("ffmpeg run failed: %w", err)
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		if errBuf.Len() > 0 {
			return nil, fmt.Errorf("ffmpeg timeout, stderr: %s", errBuf.String())
		}
		return nil, fmt.Errorf("ffmpeg timeout")
	}

	return out.Bytes(), nil
}
