package gb28181

import (
	"fmt"
	"strconv"
)

type InviteOptions struct {
	Start     int
	End       int
	Scale     float64 // 回放倍速（1.0=正常，2.0=2倍速等），仅回放有效
	ssrc      string
	SSRC      uint32
	MediaPort uint16
}

func (o InviteOptions) IsLive() bool {
	return o.Start == 0 || o.End == 0
}

func (o InviteOptions) String() string {
	return fmt.Sprintf("t=%d %d", o.Start, o.End)
}

func (o *InviteOptions) CreateSSRC(serial string, number uint16) {
	//不按gb生成标准,取ID最后六位，然后按顺序生成，一个channel最大999
	o.ssrc = fmt.Sprintf("%d%s%03d", 0, serial, number)
	_ssrc, _ := strconv.ParseInt(o.ssrc, 10, 0)
	o.SSRC = uint32(_ssrc)
}
