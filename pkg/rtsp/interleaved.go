// Copyright 2020, Chef.  All rights reserved.
// https://github.com/q191201771/lal
//
// Use of this source code is governed by a MIT-style license
// that can be found in the License file.
//
// Author: Chef (191201771@qq.com)

package rtsp

import (
	"bufio"
	"io"

	"github.com/q191201771/naza/pkg/bele"
)

// rfc2326 10.12 Embedded (Interleaved) Binary Data

func readInterleaved(r *bufio.Reader) (isInterleaved bool, packet []byte, channel uint8, err error) {
	flag, err := r.ReadByte()
	if err != nil {
		return false, nil, 0, err
	}

	if flag != Interleaved {
		_ = r.UnreadByte()
		return false, nil, 0, nil
	}

	channel, err = r.ReadByte()
	if err != nil {
		return false, nil, 0, err
	}
	// 优化：使用栈上分配的小缓冲区，减少堆分配
	var rtpLenBuf [2]byte
	_, err = io.ReadFull(r, rtpLenBuf[:])
	if err != nil {
		return false, nil, 0, err
	}
	rtpLen := int(bele.BeUint16(rtpLenBuf[:]))
	// 健壮性：检查RTP包大小，防止恶意或损坏的数据导致内存分配过大
	const maxRtpPacketSize = 65507 // UDP最大包大小 - 8字节UDP头
	if rtpLen < 0 || rtpLen > maxRtpPacketSize {
		return false, nil, 0, io.ErrUnexpectedEOF
	}
	// 优化：根据实际大小分配，避免浪费
	rtpBuf := make([]byte, rtpLen)
	_, err = io.ReadFull(r, rtpBuf)
	if err != nil {
		return false, nil, 0, err
	}

	return true, rtpBuf, channel, nil
}

func packInterleaved(channel int, rtpPacket []byte) []byte {
	// 健壮性：检查输入参数
	if rtpPacket == nil {
		rtpPacket = []byte{} // 防止nil指针
	}
	if channel < 0 || channel > 255 {
		channel = 0 // 防止无效channel
	}

	// 优化：预分配精确大小，避免浪费
	packetLen := len(rtpPacket)
	ret := make([]byte, 4+packetLen)
	ret[0] = Interleaved
	ret[1] = uint8(channel)
	bele.BePutUint16(ret[2:], uint16(packetLen))
	// 优化：使用copy而不是循环，性能更好
	if packetLen > 0 {
		copy(ret[4:], rtpPacket)
	}
	return ret
}
