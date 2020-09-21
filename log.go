package raft

// LogType describes various types of log entries.
// 日志的几种类型
type LogType uint8

const (
	// LogCommand is applied to a user FSM.
	// FSM 的命令
	LogCommand LogType = iota

	// LogNoop is used to assert leadership.
	// 检测领导关系
	LogNoop

	// LogAddPeerDeprecated is used to add a new peer. This should only be used with
	// older protocol versions designed to be compatible with unversioned
	// Raft servers. See comments in config.go for details.
	// LogAddPeer: 添加节点协议的版本小于集群协议版本
	LogAddPeerDeprecated

	// LogRemovePeerDeprecated is used to remove an existing peer. This should only be
	// used with older protocol versions designed to be compatible with
	// unversioned Raft servers. See comments in config.go for details.
	// LogRemovePeer: 删除节点协议的版本小于集群协议版本
	LogRemovePeerDeprecated

	// LogBarrier is used to ensure all preceding operations have been
	// applied to the FSM. It is similar to LogNoop, but instead of returning
	// once committed, it only returns once the FSM manager acks it. Otherwise
	// it is possible there are operations committed but not yet applied to
	// the FSM.
	// LogBarrier 确保之前的操作全部配应用到FSM。类似于LogNoop，后者返回一次提交，而她返回FSM管理确认了消息。
	// 不然，可能会出现操作提交但是没有应用到FSM的情况。
	LogBarrier

	// LogConfiguration establishes a membership change configuration. It is
	// created when a server is added, removed, promoted, etc. Only used
	// when protocol version 1 or greater is in use.
	// 配置变更。在服务添加，移除，提名(???)等时会创建
	// 协议版本大于等于1时才使用。
	LogConfiguration
)

// Log entries are replicated to all members of the Raft cluster
// and form the heart of the replicated state machine.
// Log 用于在集群节点间复制数据，是复制状态机的核心。
type Log struct {
	// Index holds the index of the log entry.
	// 日志的序号
	Index uint64

	// Term holds the election term of the log entry.
	// 日志所在的选举任期号
	Term uint64

	// Type holds the type of the log entry.
	// 日志类型
	Type LogType

	// Data holds the log entry's type-specific data.
	Data []byte
}

// LogStore is used to provide an interface for storing
// and retrieving logs in a durable fashion.
// LogStore 日志存储接口，用于刷写/获取日志，以及获取第一个和最后一个日志索引
type LogStore interface {
	// FirstIndex returns the first index written. 0 for no entries.
	FirstIndex() (uint64, error)

	// LastIndex returns the last index written. 0 for no entries.
	LastIndex() (uint64, error)

	// GetLog gets a log entry at a given index.
	GetLog(index uint64, log *Log) error

	// StoreLog stores a log entry.
	StoreLog(log *Log) error

	// StoreLogs stores multiple log entries.
	StoreLogs(logs []*Log) error

	// DeleteRange deletes a range of log entries. The range is inclusive.
	DeleteRange(min, max uint64) error
}
