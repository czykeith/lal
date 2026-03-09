package gb28181

// GB28181Config 与 lalmax 一致的 GB28181 配置，用于本包内部；由 logic 层从 Gb28181Config 转换后传入。
type GB28181Config struct {
	Enable            bool               `json:"enable"`
	ListenAddr        string             `json:"listen_addr"`
	SipIP             string             `json:"sip_ip"`
	SipPort           uint16             `json:"sip_port"`
	Serial            string             `json:"serial"`
	Realm             string             `json:"realm"`
	Username          string             `json:"username"`
	Password          string             `json:"password"`
	KeepaliveInterval int                `json:"keepalive_interval"`
	QuickLogin        bool               `json:"quick_login"`
	MediaConfig       GB28181MediaConfig `json:"media_config"`
}

// GB28181MediaConfig 媒体服务配置
type GB28181MediaConfig struct {
	MediaIp               string `json:"media_ip"`
	ListenPort            uint16 `json:"listen_port"`
	MultiPortMaxIncrement uint16 `json:"multi_port_max_increment"`
}
