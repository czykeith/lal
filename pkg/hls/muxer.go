// Copyright 2020, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package hls

import (
	"bytes"
	"fmt"

	"github.com/q191201771/naza/pkg/nazaerrors"

	"github.com/q191201771/lal/pkg/mpegts"

	"github.com/q191201771/lal/pkg/base"
)

type IMuxerObserver interface {
	OnHlsMakeTs(info base.HlsMakeTsInfo)

	// OnFragmentOpen
	//
	// 内部决定开启新的fragment切片，将该事件通知给上层
	//
	// TODO(chef): [refactor] 考虑用OnHlsMakeTs替代OnFragmentOpen 202206
	//
	OnFragmentOpen()
}

// MuxerConfig
//
// 各字段含义见文档： https://pengrl.com/lal/#/ConfigBrief
type MuxerConfig struct {
	OutPath            string `json:"out_path"`
	FragmentDurationMs int    `json:"fragment_duration_ms"`
	FragmentNum        int    `json:"fragment_num"`
	DeleteThreshold    int    `json:"delete_threshold"`
	CleanupMode        int    `json:"cleanup_mode"` // TODO chef: lalserver的模式1的逻辑是在上层做的，应该重构到hls模块中

	// MaxFragmentDurationMs 单个 TS 在 m3u8 中允许的最大时长（毫秒）上界，用于避免长期无关键帧 boundary 时单文件无限增长。
	// 0 表示取 2 * fragment_duration_ms（若 fragment_duration_ms 无效则按 3000ms 推算双倍）。
	// 达到该上界后不会立即在非关键帧切开，而是标记在下一视频关键帧 boundary 再切，并带 #EXT-X-DISCONTINUITY。
	MaxFragmentDurationMs int `json:"max_fragment_duration_ms"`
}

const (
	CleanupModeNever    = 0
	CleanupModeInTheEnd = 1
	CleanupModeAsap     = 2
)

// Muxer
//
// 输入mpegts流，输出hls(m3u8+ts)至文件中
type Muxer struct {
	UniqueKey string

	streamName                string // const after init
	outPath                   string // const after init
	playlistFilename          string // const after init
	playlistFilenameBak       string // const after init
	recordPlayListFilename    string // const after init
	recordPlayListFilenameBak string // const after init

	config   *MuxerConfig
	observer IMuxerObserver

	fragment Fragment
	videoCc  uint8
	audioCc  uint8

	// 初始值为false，调用openFragment时设置为true，调用closeFragment时设置为false
	// 整个对象关闭时设置为false
	// 中途切换Fragment时，调用close后会立即调用open
	opened bool

	fragTs                uint64 // 新建立fragment时的时间戳，毫秒 * 90
	recordMaxFragDuration float64

	nfrags int            // 该值代表直播m3u8列表中ts文件的数量
	frag   int            // frag 写入m3u8的EXT-X-MEDIA-SEQUENCE字段
	frags  []fragmentInfo // frags TS文件的固定大小环形队列，记录TS的信息

	patpmt []byte

	// 是否已收到过视频轨（用于切片时间轴：有视频后仅按视频时间戳推进切片决策，避免与音频 PTS 混用导致抖动）
	videoSeen bool
	// 当前分片内观察到的最大时间戳（90kHz），用于 close 时校准 EXTINF
	fragLastTs uint64
	// 分片已超过 MaxFragmentDurationMs 上界，等待下一关键帧 boundary 再切（有视频时）
	forceCutOnNextKey bool
	// 纯音频时尚无视频轨，超长分片在下一 audio boundary 再切（不等视频关键帧）
	forceCutOnNextAudioBoundary bool
}

// 记录fragment的一些信息，注意，写m3u8文件时可能还需要用到历史fragment的信息
type fragmentInfo struct {
	id       int     // fragment的自增序号
	duration float64 // 当前fragment中数据的时长，单位秒
	discont  bool    // #EXT-X-DISCONTINUITY
	filename string
}

// NewMuxer
//
// @param observer 可以为nil，如果不为nil，TS流将回调给上层
func NewMuxer(streamName string, config *MuxerConfig, observer IMuxerObserver) *Muxer {
	uk := base.GenUkHlsMuxer()
	op := PathStrategy.GetMuxerOutPath(config.OutPath, streamName)
	playlistFilename := PathStrategy.GetLiveM3u8FileName(op, streamName)
	recordPlaylistFilename := PathStrategy.GetRecordM3u8FileName(op, streamName)
	playlistFilenameBak := fmt.Sprintf("%s.bak", playlistFilename)
	recordPlaylistFilenameBak := fmt.Sprintf("%s.bak", recordPlaylistFilename)
	m := &Muxer{
		UniqueKey:                 uk,
		streamName:                streamName,
		outPath:                   op,
		playlistFilename:          playlistFilename,
		playlistFilenameBak:       playlistFilenameBak,
		recordPlayListFilename:    recordPlaylistFilename,
		recordPlayListFilenameBak: recordPlaylistFilenameBak,
		config:                    config,
		observer:                  observer,
	}
	m.makeFrags()
	Log.Infof("[%s] lifecycle new hls muxer. muxer=%p, streamName=%s", uk, m, streamName)
	return m
}

func (m *Muxer) Start() {
	Log.Infof("[%s] start hls muxer.", m.UniqueKey)
	m.ensureDir()
}

func (m *Muxer) Dispose() {
	Log.Infof("[%s] lifecycle dispose hls muxer.", m.UniqueKey)
	if err := m.closeFragment(true); err != nil {
		Log.Errorf("[%s] close fragment error. err=%+v", m.UniqueKey, err)
	}
}

// ---------------------------------------------------------------------------------------------------------------------

// OnPatPmt OnTsPackets
//
// 实现 remux.IRtmp2MpegtsRemuxerObserver，方便直接将 remux.Rtmp2MpegtsRemuxer 的数据喂入 hls.Muxer
func (m *Muxer) OnPatPmt(b []byte) {
	m.FeedPatPmt(b)
}

func (m *Muxer) OnTsPackets(tsPackets []byte, frame *mpegts.Frame, boundary bool) {
	m.FeedMpegts(tsPackets, frame, boundary)
}

// ---------------------------------------------------------------------------------------------------------------------

func (m *Muxer) FeedPatPmt(b []byte) {
	m.patpmt = b
}

func (m *Muxer) FeedMpegts(tsPackets []byte, frame *mpegts.Frame, boundary bool) {
	//Log.Debugf("> FeedMpegts. boundary=%v, frame=%p, sid=%d", boundary, frame, frame.Sid)
	// 支持：纯视频、纯音频、音视频。纯视频时仅有 StreamIdVideo 帧，boundary 通常由 remux 在关键帧上置位。
	if frame.Sid == mpegts.StreamIdVideo {
		m.videoSeen = true
	}
	clockTs := m.fragmentClockTs(frame)
	if err := m.updateFragment(clockTs, boundary, frame); err != nil {
		Log.Errorf("[%s] update fragment error. err=%+v", m.UniqueKey, err)
		return
	}
	if frame.Sid == mpegts.StreamIdAudio {
		if !m.opened {
			Log.Warnf("[%s] FeedMpegts A not opened. boundary=%t", m.UniqueKey, boundary)
			return
		}
	} else {
		if !m.opened {
			// 走到这，可能是第一个包并且boundary为false
			Log.Warnf("[%s] FeedMpegts V not opened. boundary=%t, key=%t", m.UniqueKey, boundary, frame.Key)
			return
		}
	}

	if err := m.fragment.WriteFile(tsPackets); err != nil {
		Log.Errorf("[%s] fragment write error. err=%+v", m.UniqueKey, err)
		return
	}
}

// ---------------------------------------------------------------------------------------------------------------------

func (m *Muxer) OutPath() string {
	return m.outPath
}

// ---------------------------------------------------------------------------------------------------------------------

// updateFragment 决定是否开启新的TS切片文件（注意，可能已经有TS切片，也可能没有，这是第一个切片）
//
// @param boundary: 调用方认为可能是开启新TS切片的时间点
//
// @param frame: 内部只在打日志时使用
//
// @return: 理论上，只有文件操作失败才会返回错误
func (m *Muxer) updateFragment(ts uint64, boundary bool, frame *mpegts.Frame) error {
	discont := true

	if m.opened {
		f := m.getCurrFrag()
		discont = false

		// 先推进「分片内最大时间戳」，再用 (fragLastTs-fragTs) 更新时长估计。
		// 音视频都贡献 fragLastTs，避免「先音频开片、视频 DTS 仍小于 fragTs」时 f.duration 卡住，
		// 导致首段/后续段迟迟达不到 fragment_duration、影响秒开与 m3u8 更新节奏。
		m.updateFragLastTs(ts)
		if m.fragLastTs > m.fragTs {
			span := float64(m.fragLastTs-m.fragTs) / 90000
			if span > f.duration {
				f.duration = span
			}
		}

		// 时间戳异常强制切：仅在 drivesFragmentTimeline 帧上判断（纯视频时帧帧均为视频，等价全程检测；纯音频阶段用音频帧）
		if m.drivesFragmentTimeline(frame) {
			if m.shouldForceSplitByTimestamp(ts) {
				Log.Warnf("[%s] force fragment split. fragTs=%d, ts=%d, frame=%s", m.UniqueKey, m.fragTs, ts, frame.DebugString())
				m.forceCutOnNextKey = false
				m.forceCutOnNextAudioBoundary = false
				if err := m.closeFragment(false); err != nil {
					return err
				}
				if err := m.openFragment(ts, true); err != nil {
					return err
				}
				m.updateFragLastTs(ts)
				return nil
			}
		}

		// 超过单段时长上界：有视频时等下一「关键帧 boundary」再切（纯视频流即下一 Key）；无视频（纯音频）时不能等关键帧，改由下一 audio boundary 直接切。
		if f.duration >= m.maxFragmentDurationSec() {
			if m.videoSeen {
				m.forceCutOnNextKey = true
			} else {
				m.forceCutOnNextAudioBoundary = true
			}
		}

		// 未达到目标分片时长则不在此处切分，除非已排队「下一关键帧强制切」或「纯音频下一 boundary 强制切」
		if f.duration < m.fragmentTargetSec() {
			if !(boundary && (m.forceCutOnNextKey || m.forceCutOnNextAudioBoundary)) {
				return nil
			}
		}
	}

	if boundary {
		needCut := false
		discontNext := true
		if m.opened {
			f := m.getCurrFrag()
			discontNext = discont
			if m.forceCutOnNextKey {
				needCut = true
				discontNext = true
				m.forceCutOnNextKey = false
			} else if m.forceCutOnNextAudioBoundary {
				needCut = true
				discontNext = true
				m.forceCutOnNextAudioBoundary = false
			} else if f.duration >= m.fragmentTargetSec() {
				needCut = true
			}
		} else {
			needCut = true
			discontNext = true
		}

		if needCut {
			if err := m.closeFragment(false); err != nil {
				return err
			}
			if err := m.openFragment(ts, discontNext); err != nil {
				return err
			}
		}
	}

	m.updateFragLastTs(ts)
	return nil
}

// fragmentClockTs 用于 HLS 切片决策的时间戳（90kHz）：视频用 DTS（与送入顺序单调一致）；纯视频时仅有视频帧。音频用 PTS。
func (m *Muxer) fragmentClockTs(frame *mpegts.Frame) uint64 {
	if frame.Sid == mpegts.StreamIdAudio {
		return frame.Pts
	}
	return frame.Dts
}

// drivesFragmentTimeline 为 true 时，本帧参与「时间戳强制切」判断：纯视频时恒为视频帧；有音频且已有视频后仅视频帧；纯音频阶段为音频帧。
func (m *Muxer) drivesFragmentTimeline(frame *mpegts.Frame) bool {
	if frame.Sid == mpegts.StreamIdVideo {
		return true
	}
	return !m.videoSeen
}

// safetyFragmentDurationMs 仅用于时间戳强制切分倍率、max 片长默认值；避免 fragment_duration_ms==0 时乘数为 0。
// 与 fragmentTargetSec 解耦：配置为 0 时仍保持原版「仅靠 boundary 切分、不按目标时长阻塞」的语义，利于秒开/GOP 对齐。
func (m *Muxer) safetyFragmentDurationMs() int {
	if m.config.FragmentDurationMs <= 0 {
		return 3000
	}
	return m.config.FragmentDurationMs
}

func (m *Muxer) fragmentTargetSec() float64 {
	if m.config.FragmentDurationMs <= 0 {
		return 0
	}
	return float64(m.config.FragmentDurationMs) / 1000
}

func (m *Muxer) maxFragmentDurationSec() float64 {
	maxMs := m.config.MaxFragmentDurationMs
	if maxMs <= 0 {
		if m.config.FragmentDurationMs <= 0 {
			maxMs = m.safetyFragmentDurationMs() * 2
		} else {
			maxMs = m.config.FragmentDurationMs * 2
		}
	}
	return float64(maxMs) / 1000
}

func (m *Muxer) shouldForceSplitByTimestamp(ts uint64) bool {
	fdMs := m.safetyFragmentDurationMs()
	maxfraglen := uint64(fdMs * 90 * 10)
	return (ts > m.fragTs && ts-m.fragTs > maxfraglen) || (m.fragTs > ts && m.fragTs-ts > negMaxfraglen)
}

func (m *Muxer) updateFragLastTs(ts uint64) {
	if !m.opened {
		return
	}
	if ts > m.fragLastTs {
		m.fragLastTs = ts
	}
}

// finalizeClosingFragmentDuration 在关闭文件前用整段内最大时间戳校准 EXTINF，减轻与真实媒体跨度不一致的问题
func (m *Muxer) finalizeClosingFragmentDuration() {
	f := m.getCurrFrag()
	if m.fragLastTs > m.fragTs {
		d := float64(m.fragLastTs-m.fragTs) / 90000
		if d > f.duration {
			f.duration = d
		}
	}
}

// openFragment
//
// @param discont: 不连续标志，会在m3u8文件的fragment前增加`#EXT-X-DISCONTINUITY`
//
// @return: 理论上，只有文件操作失败才会返回错误
func (m *Muxer) openFragment(ts uint64, discont bool) error {
	if m.opened {
		return nazaerrors.Wrap(base.ErrHls)
	}

	id := m.getFragmentId()

	filename := PathStrategy.GetTsFileName(m.streamName, id, int(Clock.Now().UnixNano()/1e6))
	filenameWithPath := PathStrategy.GetTsFileNameWithPath(m.outPath, filename)

	if err := m.fragment.OpenFile(filenameWithPath); err != nil {
		return err
	}

	if err := m.fragment.WriteFile(m.patpmt); err != nil {
		return err
	}

	m.opened = true

	frag := m.getCurrFrag()
	frag.discont = discont
	frag.id = id
	frag.filename = filename
	frag.duration = 0

	m.fragTs = ts
	m.fragLastTs = ts

	if m.observer == nil {
		return nil
	}

	// nrm said: start fragment with audio to make iPhone happy
	m.observer.OnFragmentOpen()

	m.observer.OnHlsMakeTs(base.HlsMakeTsInfo{
		Event:          "open",
		StreamName:     m.streamName,
		Cwd:            base.GetWd(),
		TsFile:         filenameWithPath,
		LiveM3u8File:   m.playlistFilename,
		RecordM3u8File: m.recordPlayListFilename,
		Id:             id,
		Duration:       frag.duration,
	})

	return nil
}

// closeFragment
//
// @return: 理论上，只有文件操作失败才会返回错误
func (m *Muxer) closeFragment(isLast bool) error {
	if !m.opened {
		// 注意，首次调用closeFragment时，有可能opened为false
		return nil
	}

	m.finalizeClosingFragmentDuration()

	if err := m.fragment.CloseFile(); err != nil {
		return err
	}

	m.opened = false

	// 更新序号，为下个分片做准备
	// 注意，后面使用序号的逻辑，都依赖该处
	m.incrFrag()

	m.writePlaylist(isLast)

	if m.config.CleanupMode == CleanupModeNever || m.config.CleanupMode == CleanupModeInTheEnd {
		m.writeRecordPlaylist()
	}
	if m.config.CleanupMode == CleanupModeAsap {
		frag := m.getDeleteFrag()
		if frag.filename != "" {
			filenameWithPath := PathStrategy.GetTsFileNameWithPath(m.outPath, frag.filename)
			if err := fslCtx.Remove(filenameWithPath); err != nil {
				Log.Warnf("[%s] remove stale fragment file failed. filename=%s, err=%+v", m.UniqueKey, filenameWithPath, err)
			}
		}
	}

	if m.observer == nil {
		return nil
	}

	currFrag := m.getClosedFrag()
	m.observer.OnHlsMakeTs(base.HlsMakeTsInfo{
		Event:          "close",
		StreamName:     m.streamName,
		Cwd:            base.GetWd(),
		TsFile:         PathStrategy.GetTsFileNameWithPath(m.outPath, currFrag.filename),
		LiveM3u8File:   m.playlistFilename,
		RecordM3u8File: m.recordPlayListFilename,
		Id:             currFrag.id,
		Duration:       currFrag.duration,
	})
	return nil
}

func (m *Muxer) writeRecordPlaylist() {
	// 找出整个直播流从开始到结束最大的分片时长
	currFrag := m.getClosedFrag()
	if currFrag.duration > m.recordMaxFragDuration {
		m.recordMaxFragDuration = currFrag.duration + 0.5
	}

	fragLines := fmt.Sprintf("#EXTINF:%.3f,\n%s\n", currFrag.duration, currFrag.filename)

	content, err := fslCtx.ReadFile(m.recordPlayListFilename)
	if err == nil {
		// m3u8文件已经存在

		content = bytes.TrimSuffix(content, []byte("#EXT-X-ENDLIST\n"))
		content, err = updateTargetDurationInM3u8(content, int(m.recordMaxFragDuration))
		if err != nil {
			Log.Errorf("[%s] update target duration failed. err=%+v", m.UniqueKey, err)
			return
		}

		if currFrag.discont {
			content = append(content, []byte("#EXT-X-DISCONTINUITY\n")...)
		}

		content = append(content, []byte(fragLines)...)
		content = append(content, []byte("#EXT-X-ENDLIST\n")...)
	} else {
		// m3u8文件不存在
		var buf bytes.Buffer
		buf.WriteString("#EXTM3U\n")
		buf.WriteString("#EXT-X-VERSION:3\n")
		buf.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(m.recordMaxFragDuration)))
		buf.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n\n", 0))

		if currFrag.discont {
			buf.WriteString("#EXT-X-DISCONTINUITY\n")
		}

		buf.WriteString(fragLines)
		buf.WriteString("#EXT-X-ENDLIST\n")

		content = buf.Bytes()
	}

	if err := writeM3u8File(content, m.recordPlayListFilename, m.recordPlayListFilenameBak); err != nil {
		Log.Errorf("[%s] write record m3u8 file error. err=%+v", m.UniqueKey, err)
	}
}

func (m *Muxer) writePlaylist(isLast bool) {
	// 找出时长最长的fragment
	maxFrag := m.fragmentTargetSec()
	m.iterateFragsInPlaylist(func(frag *fragmentInfo) {
		if frag.duration > maxFrag {
			maxFrag = frag.duration + 0.5
		}
	})

	// TODO chef 优化这块buffer的构造
	var buf bytes.Buffer
	buf.WriteString("#EXTM3U\n")
	buf.WriteString("#EXT-X-VERSION:3\n")
	buf.WriteString("#EXT-X-ALLOW-CACHE:NO\n")
	buf.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", int(maxFrag)))
	buf.WriteString(fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n\n", m.extXMediaSeq()))

	m.iterateFragsInPlaylist(func(frag *fragmentInfo) {
		if frag.discont {
			buf.WriteString("#EXT-X-DISCONTINUITY\n")
		}

		buf.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n%s\n", frag.duration, frag.filename))
	})

	if isLast {
		buf.WriteString("#EXT-X-ENDLIST\n")
	}

	if err := writeM3u8File(buf.Bytes(), m.playlistFilename, m.playlistFilenameBak); err != nil {
		Log.Errorf("[%s] write live m3u8 file error. err=%+v", m.UniqueKey, err)
	}
}

func (m *Muxer) ensureDir() {
	// 注意，如果路径已经存在，则啥也不干
	err := fslCtx.MkdirAll(m.outPath, 0777)
	Log.Assert(nil, err)
}

// ---------------------------------------------------------------------------------------------------------------------

func (m *Muxer) fragsCapacity() int {
	return m.config.FragmentNum + m.config.DeleteThreshold + 1
}

func (m *Muxer) makeFrags() {
	m.frags = make([]fragmentInfo, m.fragsCapacity())
}

// getFragmentId 获取+1递增的序号
func (m *Muxer) getFragmentId() int {
	return m.frag + m.nfrags
}

func (m *Muxer) getFrag(n int) *fragmentInfo {
	return &m.frags[(m.frag+n)%m.fragsCapacity()]
}

func (m *Muxer) incrFrag() {
	// nfrags增长到config.FragmentNum后，就增长frag
	if m.nfrags == m.config.FragmentNum {
		m.frag++
	} else {
		m.nfrags++
	}
}

func (m *Muxer) getCurrFrag() *fragmentInfo {
	return m.getFrag(m.nfrags)
}

// getClosedFrag 获取当前正关闭的frag信息
func (m *Muxer) getClosedFrag() *fragmentInfo {
	// 注意，由于是在incrFrag()后调用，所以-1
	return m.getFrag(m.nfrags - 1)
}

// iterateFragsInPlaylist 遍历当前的列表，incrFrag()后调用
func (m *Muxer) iterateFragsInPlaylist(fn func(*fragmentInfo)) {
	for i := 0; i < m.nfrags; i++ {
		frag := m.getFrag(i)
		fn(frag)
	}
}

// extXMediaSeq 获取当前列表第一个TS的序号，incrFrag()后调用
func (m *Muxer) extXMediaSeq() int {
	return m.frag
}

// getDeleteFrag 获取需要删除的frag信息，incrFrag()后调用
func (m *Muxer) getDeleteFrag() *fragmentInfo {
	// 删除过期文件
	// 注意，由于前面incrFrag()了，所以此处获取的是环形队列即将被使用的位置的上一轮残留下的数据
	// 举例：
	// config.FragmentNum=6, config.DeleteThreshold=1
	// 环形队列大小为 6+1+1 = 8
	// frags范围为[0, 7]
	//
	// nfrags=1, frag=0 时，本轮被关闭的文件实际为0号文件，此时不会删除过期文件（因为1号文件还没有生成），存在文件为0
	// nfrags=6, frag=0 时，本轮被关闭的文件实际为5号文件，此时不会删除过期文件，存在文件为[0, 5]
	// nfrags=6, frag=1 时，本轮被关闭的文件实际为6号文件，此时不会删除过期文件，存在文件为[0, 6]
	// nfrags=6, frag=2 时，本轮被关闭的文件实际为7号文件，此时删除0号文件，存在文件为[1, 7]
	// nfrags=6, frag=3 时，本轮被关闭的文件实际为8号文件，此时删除1号文件，存在文件为[2, 8]
	// ...
	//
	// 注意，实际磁盘的情况会再多一个TS文件，因为下一个TS文件会在随后立即创建，并逐渐写入数据，比如拿上面其中一个例子
	// nfrags=6, frag=3 时，实际磁盘的情况为 [2, 9]
	//
	return m.getFrag(m.nfrags)
}
