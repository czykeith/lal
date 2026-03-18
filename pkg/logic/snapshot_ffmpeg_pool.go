package logic

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/q191201771/lal/pkg/base"
)

// FfmpegJpegPool 维护一组常驻 ffmpeg worker，用于将 AnnexB(H264/H265) 解码为 JPEG。
//
// 设计目标：
// - 避免每次截图都创建/销毁进程导致抖动
// - 控制并发上限，防止截图流量拖垮主进程
//
// 注意：每个 worker 是“单进程串行”模型：一次写入一帧 AnnexB，读取一张 JPEG，按顺序处理请求。
type FfmpegJpegPool struct {
	h264 chan *ffmpegJpegWorker
	hevc chan *ffmpegJpegWorker
}

func NewFfmpegJpegPool(poolSize int) *FfmpegJpegPool {
	if poolSize < 0 {
		poolSize = 0
	}
	if poolSize == 0 {
		return nil
	}
	p := &FfmpegJpegPool{
		h264: make(chan *ffmpegJpegWorker, poolSize),
		hevc: make(chan *ffmpegJpegWorker, poolSize),
	}
	for i := 0; i < poolSize; i++ {
		p.h264 <- newFfmpegJpegWorker("h264")
		p.hevc <- newFfmpegJpegWorker("hevc")
	}
	return p
}

func (p *FfmpegJpegPool) Decode(ctx context.Context, frame *SnapshotFrame) ([]byte, error) {
	if frame == nil || len(frame.Data) == 0 {
		return nil, fmt.Errorf("empty snapshot frame")
	}
	codec := payloadTypeToCodec(frame.PayloadType)
	if codec == "" {
		return nil, fmt.Errorf("unsupported payload type for snapshot: %d", frame.PayloadType)
	}

	// poolSize=0 时退化为单次拉起 ffmpeg（兼容，但不推荐）
	if p == nil {
		return convertAnnexbToJPEG(frame)
	}

	var ch chan *ffmpegJpegWorker
	switch codec {
	case "h264":
		ch = p.h264
	case "hevc":
		ch = p.hevc
	default:
		return nil, fmt.Errorf("unknown codec: %s", codec)
	}

	var w *ffmpegJpegWorker
	select {
	case w = <-ch:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	jpg, err := w.Decode(ctx, frame.Data)

	// 归还 worker（即便失败也归还；worker 内部会按需重启）
	select {
	case ch <- w:
	default:
		// 理论上不会发生：取出一个就应该能放回去
	}
	return jpg, err
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

type ffmpegJpegWorker struct {
	codec string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	// stderr 仅做排障保留最近内容
	stderrMu sync.Mutex
	stderr   bytes.Buffer
}

func newFfmpegJpegWorker(codec string) *ffmpegJpegWorker {
	return &ffmpegJpegWorker{codec: codec}
}

func (w *ffmpegJpegWorker) Decode(ctx context.Context, annexb []byte) ([]byte, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.ensureStarted(); err != nil {
		return nil, err
	}

	// 写入一帧（通常为 SPS/PPS/VPS + IDR）。不关闭 stdin，让进程常驻。
	if _, err := w.stdin.Write(annexb); err != nil {
		_ = w.restartLocked()
		return nil, fmt.Errorf("ffmpeg stdin write failed: %w", err)
	}

	// 从 stdout 读取一张 JPEG（ffmpeg 会按解码帧输出连续 JPEG 流）
	jpg, err := w.readOneJpeg(ctx)
	if err != nil {
		_ = w.restartLocked()
		return nil, err
	}
	return jpg, nil
}

func (w *ffmpegJpegWorker) ensureStarted() error {
	if w.cmd != nil {
		return nil
	}

	// 常驻输出 MJPEG 流：每解出一帧输出一张 jpeg 到 stdout
	// 注意：依赖输入是“可独立解码的关键帧”（你们 SnapshotStore 已做 SPS/PPS 过滤）。
	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostdin",
		"-threads", "1",
		"-f", w.codec,
		"-i", "pipe:0",
		"-an", "-sn",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"pipe:1",
	}
	cmd := exec.Command("ffmpeg", args...)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return err
	}

	w.cmd = cmd
	w.stdin = stdin
	w.stdout = bufio.NewReaderSize(stdout, 256*1024)

	go w.drainStderr(stderr)
	go func() {
		// 若进程异常退出，清理状态，便于下次重启
		_ = cmd.Wait()
		w.mu.Lock()
		if w.cmd == cmd {
			w.cmd = nil
			w.stdin = nil
			w.stdout = nil
		}
		w.mu.Unlock()
	}()

	return nil
}

func (w *ffmpegJpegWorker) restartLocked() error {
	if w.cmd != nil && w.cmd.Process != nil {
		_ = w.cmd.Process.Kill()
	}
	w.cmd = nil
	w.stdin = nil
	w.stdout = nil
	return nil
}

func (w *ffmpegJpegWorker) readOneJpeg(ctx context.Context) ([]byte, error) {
	if w.stdout == nil {
		return nil, fmt.Errorf("ffmpeg stdout not ready")
	}

	// 读取直到 SOI(FFD8) 后开始缓存，再读到 EOI(FFD9) 为止。
	// 设一个上限避免异常码流/进程输出导致内存爆炸。
	const maxJpegSize = 5 * 1024 * 1024

	type state int
	const (
		stSeekSOI state = iota
		stInJpeg
	)
	s := stSeekSOI
	var last byte
	var buf bytes.Buffer
	tmp := make([]byte, 32*1024)

	deadline := time.Now().Add(3 * time.Second)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ffmpeg jpeg read timeout")
		}

		n, err := w.stdout.Read(tmp)
		if n > 0 {
			b := tmp[:n]
			for i := 0; i < len(b); i++ {
				cur := b[i]
				switch s {
				case stSeekSOI:
					if last == 0xFF && cur == 0xD8 {
						s = stInJpeg
						buf.Reset()
						_ = buf.WriteByte(0xFF)
						_ = buf.WriteByte(0xD8)
					}
				case stInJpeg:
					_ = buf.WriteByte(cur)
					if buf.Len() > maxJpegSize {
						return nil, fmt.Errorf("jpeg too large")
					}
					if last == 0xFF && cur == 0xD9 {
						return buf.Bytes(), nil
					}
				}
				last = cur
			}
		}
		if err != nil {
			return nil, fmt.Errorf("ffmpeg stdout read failed: %w", err)
		}
	}
}

func (w *ffmpegJpegWorker) drainStderr(r io.Reader) {
	// 避免 stderr pipe 堵塞；保留最后 8KB 便于排障。
	const keep = 8 * 1024
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			w.stderrMu.Lock()
			_, _ = w.stderr.Write(tmp[:n])
			if w.stderr.Len() > keep {
				b := w.stderr.Bytes()
				w.stderr.Reset()
				_, _ = w.stderr.Write(b[len(b)-keep:])
			}
			w.stderrMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}
