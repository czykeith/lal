package gb28181

// GB28181Config 与 lalmax 一致的 GB28181 配置，用于本包内部；由 logic 层从 Gb28181Config 转换后传入。
type GB28181Config struct {
	Enable                   bool               `json:"enable"`
	ListenAddr               string             `json:"listen_addr"`
	SipIP                    string             `json:"sip_ip"`
	SipPort                  uint16             `json:"sip_port"`
	Serial                   string             `json:"serial"`
	Realm                    string             `json:"realm"`
	Username                 string             `json:"username"`
	Password                 string             `json:"password"`
	KeepaliveInterval        int                `json:"keepalive_interval"`
	QuickLogin               bool               `json:"quick_login"`
	MediaConfig              GB28181MediaConfig `json:"media_config"`
	AllowNonStandardDeviceId bool               `json:"allow_non_standard_device_id"`

	// Video* 用于生成 INVITE 的 SDP（f=/fmtp 等），尽量对齐 lalmax 配置语义。
	VideoCodec     string `json:"video_codec"`     // H264/H265（默认H264）
	VideoWidth     int    `json:"video_width"`     // 分辨率宽（0表示不指定）
	VideoHeight    int    `json:"video_height"`    // 分辨率高（0表示不指定）
	VideoBitrate   int    `json:"video_bitrate"`   // kbps（0表示不指定）
	VideoFramerate int    `json:"video_framerate"` // fps（0表示不指定）
	VideoProfile   string `json:"video_profile"`   // baseline/main/high（可选）
	VideoLevel     string `json:"video_level"`     // 例如 3.1/4.0（可选）

	// 断线重试恢复（仅对实时点播生效，回放不自动重试）
	AutoRetryOnDisconnect bool `json:"auto_retry_on_disconnect"` // 拉流断线后是否自动重试 Invite，默认 false
	RetryMaxCount         int  `json:"retry_max_count"`          // 最大重试次数，0=不重试，-1=无限重试，默认 3
	RetryFirstDelayMs     int  `json:"retry_first_delay_ms"`     // 首次重试延迟（毫秒），默认 3000
	RetryMaxDelayMs       int  `json:"retry_max_delay_ms"`       // 退避上限（毫秒），默认 60000
}

// GB28181MediaConfig 媒体服务配置
type GB28181MediaConfig struct {
	MediaIp               string `json:"media_ip"`
	ListenPort            uint16 `json:"listen_port"`
	MultiPortMaxIncrement uint16 `json:"multi_port_max_increment"`
}
