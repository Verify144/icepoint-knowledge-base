package protocol

const (
	IDText               uint32 = 0x09
	IDCommandRequest     uint32 = 0x4d
	IDCommandBlockUpdate uint32 = 0x4e
	IDCommandOutput      uint32 = 0x4f
	IDUpdateBlock        uint32 = 0x15
	IDSubChunkRequest    uint32 = 0xaf
	IDSubChunkResponse   uint32 = 0xae
	IDLevelChunk         uint32 = 0x3a // 服务端主动下发整区块；真机语料里每条都以此 ID 观测到

	// 0x9D 不是 RequestPermissions —— 它是 MotionPredictionHints（服务端单向通知，
	// 不该回执）。真正的编号取自一份带网易字段的实现，并已用多个独立实测锚点校验：
	// 0x4F=CommandOutput、0x9C=PacketViolationWarning（我们解出的违规文本）、
	// 0x71=SetLocalPlayerAsInitialised（服务端点名的 113）、0x90=PlayerAuthInput（144）、
	// 0xAF/0xAE=我们真机跑通的往返。整套对齐后：
	IDMotionPredictionHints uint32 = 0x9d
	IDRequestPermissions    uint32 = 0xb9
	IDUpdateAbilities       uint32 = 0xbb
)

