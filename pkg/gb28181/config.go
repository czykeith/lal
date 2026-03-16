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

	// CatalogQueryInterval 下级设备目录（Catalog）定时查询间隔（秒）。
	// 若配置值 <=0，则在 gb28181Server 中采用默认值 180 秒。
	CatalogQueryInterval int `json:"catalog_query_interval"`

	// 上级 GB28181 消息监听端口（中间平台模式下对上级开放的 SIP 端口），仅供自定义 REGISTER 使用。
	// 设备接入仍使用 sip_port。本字段主要用于自定义 UDP 客户端绑定本地端口，避免与设备侧 5060 端口混用。
	// 默认 5061。
	UpstreamSipPort uint16 `json:"upstream_sip_port"`

	// Upstreams 上级 GB28181 平台列表配置。
	// 本服务在这些上级眼中以“设备”身份存在，用于实现级联转推。
	Upstreams []GB28181UpstreamConfig `json:"upstreams"`
}

// GB28181MediaConfig 媒体服务配置
type GB28181MediaConfig struct {
	MediaIp               string `json:"media_ip"`
	ListenPort            uint16 `json:"listen_port"`
	MultiPortMaxIncrement uint16 `json:"multi_port_max_increment"`
}

// GB28181UpstreamConfig 上级 GB28181 平台配置。
// 仅在启用级联转推时使用，由 logic 层持久化到主配置文件。
type GB28181UpstreamConfig struct {
	// ID 为逻辑唯一标识，用于 HTTP 管理、内存索引。
	ID string `json:"id"`

	Enable bool `json:"enable"`

	// 上级平台的 SIP ID、域、IP、端口。
	SipID   string `json:"sip_id"`   // 上级平台自身的国标编码（例如 34020000002000000001）
	Realm   string `json:"realm"`    // 上级平台国标域，如 3402000000
	SipIP   string `json:"sip_ip"`   // 上级平台 SIP 信令 IP
	SipPort uint16 `json:"sip_port"` // 上级平台 SIP 信令端口

	// 我方在上级平台注册时使用的设备 ID（在上级眼中，我们是一个国标设备）。
	LocalDeviceID string `json:"local_device_id"`

	// 上级平台为我们分配的认证账号。
	Username string `json:"username"`
	Password string `json:"password"`

	// 注册与心跳相关。
	RegisterValidity  int `json:"register_validity"`  // 注册有效期（秒），默认 3600
	KeepaliveInterval int `json:"keepalive_interval"` // 心跳间隔（秒），默认 60

	// 媒体参数（向上级推流时 SDP 使用）。
	MediaIP   string `json:"media_ip"`   // 本节点对上级可达的媒体 IP
	MediaPort uint16 `json:"media_port"` // 本节点对上级可达的媒体端口（单端口模式）

	// 备注信息，便于运维标注。
	Comment string `json:"comment"`
}
