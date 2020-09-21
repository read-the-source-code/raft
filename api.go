package raft

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/armon/go-metrics"
)

var (
	// ErrLeader is returned when an operation can't be completed on a
	// leader node.
	ErrLeader = errors.New("node is the leader")

	// ErrNotLeader is returned when an operation can't be completed on a
	// follower or candidate node.
	ErrNotLeader = errors.New("node is not the leader")

	// ErrLeadershipLost is returned when a leader fails to commit a log entry
	// because it's been deposed in the process.
	ErrLeadershipLost = errors.New("leadership lost while committing log")

	// ErrAbortedByRestore is returned when a leader fails to commit a log
	// entry because it's been superseded by a user snapshot restore.
	ErrAbortedByRestore = errors.New("snapshot restored while committing log")

	// ErrRaftShutdown is returned when operations are requested against an
	// inactive Raft.
	ErrRaftShutdown = errors.New("raft is already shutdown")

	// ErrEnqueueTimeout is returned when a command fails due to a timeout.
	ErrEnqueueTimeout = errors.New("timed out enqueuing operation")

	// ErrNothingNewToSnapshot is returned when trying to create a snapshot
	// but there's nothing new commited to the FSM since we started.
	ErrNothingNewToSnapshot = errors.New("nothing new to snapshot")

	// ErrUnsupportedProtocol is returned when an operation is attempted
	// that's not supported by the current protocol version.
	ErrUnsupportedProtocol = errors.New("operation not supported with current protocol version")

	// ErrCantBootstrap is returned when attempt is made to bootstrap a
	// cluster that already has state present.
	ErrCantBootstrap = errors.New("bootstrap only works on new clusters")
)

// Raft implements a Raft node.
// Raft 节点的结构体
type Raft struct {
	raftState // stage.go

	// protocolVersion is used to inter-operate with Raft servers running
	// different versions of the library. See comments in config.go for more
	// details.
	// protocolVersion 为了避免服务端和客户端协议版本不一致，可在config.go的注释查看详情。
	protocolVersion ProtocolVersion

	// applyCh is used to async send logs to the main thread to
	// be committed and applied to the FSM.
	// applyCh 用于异步地向主进程发送需要向FSM commit 和 apply 日志的通道。
	applyCh chan *logFuture

	// Configuration provided at Raft initialization
	// 供初始的化配置
	conf Config

	// FSM is the client state machine to apply commands to
	// FSM 处理客户端命令的有限状态机
	fsm FSM

	// fsmMutateCh is used to send state-changing updates to the FSM. This
	// receives pointers to commitTuple structures when applying logs or
	// pointers to restoreFuture structures when restoring a snapshot. We
	// need control over the order of these operations when doing user
	// restores so that we finish applying any old log applies before we
	// take a user snapshot on the leader, otherwise we might restore the
	// snapshot and apply old logs to it that were in the pipe.
	// fsmMutateCh 发送状态改变给FSM的通道。
	// apply 日志时接收指向commitTuple结构体的指针，恢复快照时接收指向restoreFuture结构体的指针。
	// 在处理快照恢复时，为了确保老的日志应用在快照之前，需要严格的控制操作的顺序。
	// 否则，我们可能会向快照中应用了老的日志。
	fsmMutateCh chan interface{}

	// fsmSnapshotCh is used to trigger a new snapshot being taken
	// fsmSnapshotCh 用于触发产生新快照的阻塞通道，产生快照请求后，会通过这个来等待
	fsmSnapshotCh chan *reqSnapshotFuture

	// lastContact is the last time we had contact from the
	// leader node. This can be used to gauge staleness.
	// lastContact 最后一次和leader节点通信的时间，可以用来衡量节点的鲜活度。
	lastContact     time.Time
	lastContactLock sync.RWMutex

	// Leader is the current cluster leader
	// 当前集群的 leader 节点
	leader     ServerAddress
	leaderLock sync.RWMutex

	// leaderCh is used to notify of leadership changes
	// leaderCh 通知leader关系发生改变的通道
	leaderCh chan bool

	// leaderState used only while state is leader
	// leaderState leader的状态，仅当当前节点为leader时使用
	leaderState leaderState

	// Stores our local server ID, used to avoid sending RPCs to ourself
	// 本地服务器的ID，避免发送RPC消息给自己
	localID ServerID

	// Stores our local addr
	// 本地服务地址
	localAddr ServerAddress

	// Used for our logging
	// 程序日志
	logger *log.Logger

	// LogStore provides durable storage for logs
	// 持久化存储的日志
	logs LogStore

	// Used to request the leader to make configuration changes.
	// 用于向leader申请配置变更的通道
	configurationChangeCh chan *configurationChangeFuture

	// Tracks the latest configuration and latest committed configuration from
	// the log/snapshot.
	// 最新配置
	configurations configurations

	// RPC chan comes from the transport layer
	// 通道用于传输层接收层发过来的RPC消息
	rpcCh <-chan RPC

	// Shutdown channel to exit, protected to prevent concurrent exits
	// 退出通道，防止并发退出
	shutdown     bool
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex

	// snapshots is used to store and retrieve snapshots
	// 存储和检索的快照
	snapshots SnapshotStore

	// userSnapshotCh is used for user-triggered snapshots
	// 用户触发快照通道
	userSnapshotCh chan *userSnapshotFuture

	// userRestoreCh is used for user-triggered restores of external
	// snapshots
	// 用户触发恢复快照通道
	userRestoreCh chan *userRestoreFuture

	// stable is a StableStore implementation for durable state
	// It provides stable storage for many fields in raftState
	// 持久化存储
	stable StableStore

	// The transport layer we use
	// 传输层
	trans Transport

	// verifyCh is used to async send verify futures to the main thread
	// to verify we are still the leader
	// 接收验证我们依然是leader的请求的通道
	verifyCh chan *verifyFuture

	// configurationsCh is used to get the configuration data safely from
	// outside of the main thread.
	// 接收配置的通道
	configurationsCh chan *configurationsFuture

	// bootstrapCh is used to attempt an initial bootstrap from outside of
	// the main thread.
	// 接收初始化请求的通道
	bootstrapCh chan *bootstrapFuture

	// List of observers and the mutex that protects them. The observers list
	// is indexed by an artificial ID which is used for deregistration.
	// observers 生成的ID索引，一遍注销
	// TODO: observers 是什么？
	observersLock sync.RWMutex
	observers     map[uint64]*Observer
}

// BootstrapCluster initializes a server's storage with the given cluster
// configuration. This should only be called at the beginning of time for the
// cluster, and you absolutely must make sure that you call it with the same
// configuration on all the Voter servers. There is no need to bootstrap
// Nonvoter and Staging servers.
// 初始化集群
// BootstrapCluster 函数使用提供的集群配置初始化服务端的存储。
// 仅在集群启动时调用，并且需要确保集群中所有的Voter服务都是使用同一个配置。
// 不需要启动Nonvoter和Staging服务。
//
// One sane approach is to bootstrap a single server with a configuration
// listing just itself as a Voter, then invoke AddVoter() on it to add other
// servers to the cluster.
// 合理的做法是，使用配置启动一个服务，并且让这个服务称为Voter，然后调用AddVoter()
// 添加其他服务到集群中来。
func BootstrapCluster(conf *Config, logs LogStore, stable StableStore,
	snaps SnapshotStore, trans Transport, configuration Configuration) error {
	// Validate the Raft server config.
	// 验证配置
	if err := ValidateConfig(conf); err != nil {
		return err
	}

	// Sanity check the Raft peer configuration.
	// 校验节点配置
	if err := checkConfiguration(configuration); err != nil {
		return err
	}

	// Make sure the cluster is in a clean state.
	// 确保集群状态干净
	hasState, err := HasExistingState(logs, stable, snaps)
	if err != nil {
		return fmt.Errorf("failed to check for existing state: %v", err)
	}
	if hasState {
		return ErrCantBootstrap
	}

	// Set current term to 1.
	// 设置任期为1
	if err := stable.SetUint64(keyCurrentTerm, 1); err != nil {
		return fmt.Errorf("failed to save current term: %v", err)
	}

	// Append configuration entry to log.
	// 向日志追加配置项
	entry := &Log{
		Index: 1,
		Term:  1,
	}
	if conf.ProtocolVersion < 3 {
		// 配置中协议版本小于3
		entry.Type = LogRemovePeerDeprecated
		entry.Data = encodePeers(configuration, trans)
	} else {
		// 编码配置信息
		entry.Type = LogConfiguration
		entry.Data = encodeConfiguration(configuration)
	}
	// 存储日志（配置/移除低版本）
	if err := logs.StoreLog(entry); err != nil {
		return fmt.Errorf("failed to append configuration entry to log: %v", err)
	}

	return nil
}

// RecoverCluster is used to manually force a new configuration in order to
// recover from a loss of quorum where the current configuration cannot be
// restored, such as when several servers die at the same time. This works by
// reading all the current state for this server, creating a snapshot with the
// supplied configuration, and then truncating the Raft log. This is the only
// safe way to force a given configuration without actually altering the log to
// insert any new entries, which could cause conflicts with other servers with
// different state.
// RecoverCluster 在丢失大多节点时，当前配置无法恢复，使用新配置恢复集群，
// 比如在同一时间好几个服务挂掉的情况。
// 是通过读取当前服务的所有状态，产生快照，然后截断 Raft 日志来处理。
// 这个唯一安全避免冲突的安全方式。
//
// WARNING! This operation implicitly commits all entries in the Raft log, so
// in general this is an extremely unsafe operation. If you've lost your other
// servers and are performing a manual recovery, then you've also lost the
// commit information, so this is likely the best you can do, but you should be
// aware that calling this can cause Raft log entries that were in the process
// of being replicated but not yet be committed to be committed.
// 警告！这个操作隐式提交了所有的Raft日志条目，所以通常清洗下这是一个非常不安全的操作。
// 如果你丢失了大部分节点，并且准备手动恢复，那你也丢失了提交信息，所以这也许是你的最好的处理办法，
// 但是你需要知道，一旦调用了这个方法会导致那些正在复制但没有被提交的日志会被提交。
//
// Note the FSM passed here is used for the snapshot operations and will be
// left in a state that should not be used by the application. Be sure to
// discard this FSM and any associated state and provide a fresh one when
// calling NewRaft later.
// 需要注意的是FSM，TODO：
//
// A typical way to recover the cluster is to shut down all servers and then
// run RecoverCluster on every server using an identical configuration. When
// the cluster is then restarted, and election should occur and then Raft will
// resume normal operation. If it's desired to make a particular server the
// leader, this can be used to inject a new configuration with that server as
// the sole voter, and then join up other new clean-state peer servers using
// the usual APIs in order to bring the cluster back into a known state.
// 一个典型的恢复集群的方式是停掉所有的服务，然后调用在每一个服务上使用相同的配置调用RecoverCluster方法。
// 当集群重启完成，新的选举会发生，Raft会恢复正常的操作。
// 如果需要指定 leader，使用新的配置对那个服务（给自己选举）调用这个函数，然后将其他服务接入到这个集群钟来。
func RecoverCluster(conf *Config, fsm FSM, logs LogStore, stable StableStore,
	snaps SnapshotStore, trans Transport, configuration Configuration) error {
	// Validate the Raft server config.
	// 校验配置
	if err := ValidateConfig(conf); err != nil {
		return err
	}

	// Sanity check the Raft peer configuration.
	// 校验节点配置
	if err := checkConfiguration(configuration); err != nil {
		return err
	}

	// Refuse to recover if there's no existing state. This would be safe to
	// do, but it is likely an indication of an operator error where they
	// expect data to be there and it's not. By refusing, we force them
	// to show intent to start a cluster fresh by explicitly doing a
	// bootstrap, rather than quietly fire up a fresh cluster here.
	// 无状态时拒绝恢复。虽然这个操作是安全的，但是是一个错误的征兆，
	// 因为预期有数据，结果却没有。
	// 拒绝后，我们可以显式地告知需通过启动新集群的方式来启动。
	hasState, err := HasExistingState(logs, stable, snaps)
	if err != nil {
		return fmt.Errorf("failed to check for existing state: %v", err)
	}
	if !hasState {
		return fmt.Errorf("refused to recover cluster with no initial state, this is probably an operator error")
	}

	// Attempt to restore any snapshots we find, newest to oldest.
	// 试图从新到老恢复找到的快照。
	var snapshotIndex uint64
	var snapshotTerm uint64
	snapshots, err := snaps.List()
	if err != nil {
		return fmt.Errorf("failed to list snapshots: %v", err)
	}
	for _, snapshot := range snapshots {
		// 打开快照
		_, source, err := snaps.Open(snapshot.ID)
		if err != nil {
			// Skip this one and try the next. We will detect if we
			// couldn't open any snapshots.
			continue
		}
		defer source.Close()

		// 向状态机中恢复数据
		if err := fsm.Restore(source); err != nil {
			// Same here, skip and try the next one.
			continue
		}

		// 恢复了最新（成功）的快照
		snapshotIndex = snapshot.Index
		snapshotTerm = snapshot.Term
		break
	}

	// 存在快照，但是没有恢复成功
	if len(snapshots) > 0 && (snapshotIndex == 0 || snapshotTerm == 0) {
		return fmt.Errorf("failed to restore any of the available snapshots")
	}

	// The snapshot information is the best known end point for the data
	// until we play back the Raft log entries.
	// 回放日志时，快照的这两个信息是我们目前已知的起点。
	lastIndex := snapshotIndex
	lastTerm := snapshotTerm

	// Apply any Raft log entries past the snapshot.
	// 应用日志
	lastLogIndex, err := logs.LastIndex()
	if err != nil {
		return fmt.Errorf("failed to find last log: %v", err)
	}
	// 从快照以后的日志开始应用
	for index := snapshotIndex + 1; index <= lastLogIndex; index++ {
		var entry Log
		if err := logs.GetLog(index, &entry); err != nil {
			return fmt.Errorf("failed to get log at index %d: %v", index, err)
		}
		// LogCommand 类型的需要向FSM应用
		if entry.Type == LogCommand {
			_ = fsm.Apply(&entry)
		}

		// 从日志中更新了最后的日志序号和任期
		lastIndex = entry.Index
		lastTerm = entry.Term
	}

	// Create a new snapshot, placing the configuration in as if it was
	// committed at index 1.
	// 创建新的快照，添加配置信息序号为1。
	snapshot, err := fsm.Snapshot()
	if err != nil {
		return fmt.Errorf("failed to snapshot FSM: %v", err)
	}
	// version = 1
	version := getSnapshotVersion(conf.ProtocolVersion)
	// 创建快照
	sink, err := snaps.Create(version, lastIndex, lastTerm, configuration, 1, trans)
	if err != nil {
		return fmt.Errorf("failed to create snapshot: %v", err)
	}
	// 持久化快照
	if err := snapshot.Persist(sink); err != nil {
		return fmt.Errorf("failed to persist snapshot: %v", err)
	}
	// 关闭快照
	if err := sink.Close(); err != nil {
		return fmt.Errorf("failed to finalize snapshot: %v", err)
	}

	// Compact the log so that we don't get bad interference from any
	// configuration change log entries that might be there.
	// 清空日志，避免配置提交日志带来的干扰。
	firstLogIndex, err := logs.FirstIndex()
	if err != nil {
		return fmt.Errorf("failed to get first log index: %v", err)
	}
	if err := logs.DeleteRange(firstLogIndex, lastLogIndex); err != nil {
		return fmt.Errorf("log compaction failed: %v", err)
	}

	return nil
}

// HasExistingState returns true if the server has any existing state (logs,
// knowledge of a current term, or any snapshots).
// HasExistingState 检查集群是否干净，如果存在状态(日志，任期以及快照)则返回true
func HasExistingState(logs LogStore, stable StableStore, snaps SnapshotStore) (bool, error) {
	// Make sure we don't have a current term.
	// 确保无任期
	currentTerm, err := stable.GetUint64(keyCurrentTerm)
	if err == nil {
		if currentTerm > 0 {
			return true, nil
		}
	} else {
		if err.Error() != "not found" {
			return false, fmt.Errorf("failed to read current term: %v", err)
		}
	}

	// Make sure we have an empty log.
	// 确保日志为空
	lastIndex, err := logs.LastIndex()
	if err != nil {
		return false, fmt.Errorf("failed to get last log index: %v", err)
	}
	if lastIndex > 0 {
		return true, nil
	}

	// Make sure we have no snapshots
	// 确保无快照
	snapshots, err := snaps.List()
	if err != nil {
		return false, fmt.Errorf("failed to list snapshots: %v", err)
	}
	if len(snapshots) > 0 {
		return true, nil
	}

	return false, nil
}

// NewRaft is used to construct a new Raft node. It takes a configuration, as well
// as implementations of various interfaces that are required. If we have any
// old state, such as snapshots, logs, peers, etc, all those will be restored
// when creating the Raft node.
// NewRaft 构造新的Raft节点。
// 接收的参数：配置，用于恢复时需要老的状态，快照，日志，节点等。
func NewRaft(conf *Config, fsm FSM, logs LogStore, stable StableStore, snaps SnapshotStore, trans Transport) (*Raft, error) {
	// Validate the configuration.
	// 校验配置
	if err := ValidateConfig(conf); err != nil {
		return nil, err
	}

	// Ensure we have a LogOutput.
	// 确保有标准日志输出的地方
	var logger *log.Logger
	if conf.Logger != nil {
		logger = conf.Logger
	} else {
		if conf.LogOutput == nil {
			conf.LogOutput = os.Stderr
		}
		logger = log.New(conf.LogOutput, "", log.LstdFlags)
	}

	// Try to restore the current term.
	// 尝试恢复当前任期
	currentTerm, err := stable.GetUint64(keyCurrentTerm)
	if err != nil && err.Error() != "not found" {
		return nil, fmt.Errorf("failed to load current term: %v", err)
	}

	// Read the index of the last log entry.
	// 读取最后日志的序号
	lastIndex, err := logs.LastIndex()
	if err != nil {
		return nil, fmt.Errorf("failed to find last log: %v", err)
	}

	// Get the last log entry.
	// 获取最后日志
	var lastLog Log
	if lastIndex > 0 {
		if err = logs.GetLog(lastIndex, &lastLog); err != nil {
			return nil, fmt.Errorf("failed to get last log at index %d: %v", lastIndex, err)
		}
	}

	// Make sure we have a valid server address and ID.
	// 确保服务器地址和ID合法
	protocolVersion := conf.ProtocolVersion
	localAddr := ServerAddress(trans.LocalAddr())
	localID := conf.LocalID

	// TODO (slackpad) - When we deprecate protocol version 2, remove this
	// along with the AddPeer() and RemovePeer() APIs.
	if protocolVersion < 3 && string(localID) != string(localAddr) {
		return nil, fmt.Errorf("when running with ProtocolVersion < 3, LocalID must be set to the network address")
	}

	// Create Raft struct.
	// 构造Raft结构体
	r := &Raft{
		protocolVersion:       protocolVersion,
		applyCh:               make(chan *logFuture),
		conf:                  *conf,
		fsm:                   fsm,
		fsmMutateCh:           make(chan interface{}, 128),
		fsmSnapshotCh:         make(chan *reqSnapshotFuture),
		leaderCh:              make(chan bool),
		localID:               localID,
		localAddr:             localAddr,
		logger:                logger,
		logs:                  logs,
		configurationChangeCh: make(chan *configurationChangeFuture),
		configurations:        configurations{},
		rpcCh:                 trans.Consumer(),
		snapshots:             snaps,
		userSnapshotCh:        make(chan *userSnapshotFuture),
		userRestoreCh:         make(chan *userRestoreFuture),
		shutdownCh:            make(chan struct{}),
		stable:                stable,
		trans:                 trans,
		verifyCh:              make(chan *verifyFuture, 64),
		configurationsCh:      make(chan *configurationsFuture, 8),
		bootstrapCh:           make(chan *bootstrapFuture),
		observers:             make(map[uint64]*Observer),
	}

	// Initialize as a follower.
	// 初始化为Follower
	r.setState(Follower)

	// Start as leader if specified. This should only be used
	// for testing purposes.
	// 如果配置中声明为Leader，就设置为Leader，仅供测试。
	if conf.StartAsLeader {
		r.setState(Leader)
		r.setLeader(r.localAddr)
	}

	// Restore the current term and the last log.
	// 恢复当前任期和最后日志
	r.setCurrentTerm(currentTerm)
	r.setLastLog(lastLog.Index, lastLog.Term)

	// Attempt to restore a snapshot if there are any.
	// 尝试恢复快照
	if err := r.restoreSnapshot(); err != nil {
		return nil, err
	}

	// Scan through the log for any configuration change entries.
	// 搜索最新快照之后的日志中的配置等变更
	snapshotIndex, _ := r.getLastSnapshot()
	for index := snapshotIndex + 1; index <= lastLog.Index; index++ {
		var entry Log
		if err := r.logs.GetLog(index, &entry); err != nil {
			r.logger.Printf("[ERR] raft: Failed to get log at %d: %v", index, err)
			panic(err)
		}
		r.processConfigurationLogEntry(&entry)
	}

	r.logger.Printf("[INFO] raft: Initial configuration (index=%d): %+v",
		r.configurations.latestIndex, r.configurations.latest.Servers)

	// Setup a heartbeat fast-path to avoid head-of-line
	// blocking where possible. It MUST be safe for this
	// to be called concurrently with a blocking RPC.
	// 设置心跳处理。必须并发安全
	trans.SetHeartbeatHandler(r.processHeartbeat)

	// Start the background work.
	// 启动后台工作
	r.goFunc(r.run)
	r.goFunc(r.runFSM)
	r.goFunc(r.runSnapshots)
	return r, nil
}

// restoreSnapshot attempts to restore the latest snapshots, and fails if none
// of them can be restored. This is called at initialization time, and is
// completely unsafe to call at any other time.
// restoreSnapshot 尝试恢复最新的快照。
// 在初始化的时候调用，其他时间调用非常不安全。
func (r *Raft) restoreSnapshot() error {
	snapshots, err := r.snapshots.List()
	if err != nil {
		r.logger.Printf("[ERR] raft: Failed to list snapshots: %v", err)
		return err
	}

	// Try to load in order of newest to oldest
	for _, snapshot := range snapshots {
		_, source, err := r.snapshots.Open(snapshot.ID)
		if err != nil {
			r.logger.Printf("[ERR] raft: Failed to open snapshot %v: %v", snapshot.ID, err)
			continue
		}
		defer source.Close()

		if err := r.fsm.Restore(source); err != nil {
			r.logger.Printf("[ERR] raft: Failed to restore snapshot %v: %v", snapshot.ID, err)
			continue
		}

		// Log success
		r.logger.Printf("[INFO] raft: Restored from snapshot %v", snapshot.ID)

		// Update the lastApplied so we don't replay old logs
		r.setLastApplied(snapshot.Index)

		// Update the last stable snapshot info
		r.setLastSnapshot(snapshot.Index, snapshot.Term)

		// Update the configuration
		if snapshot.Version > 0 {
			r.configurations.committed = snapshot.Configuration
			r.configurations.committedIndex = snapshot.ConfigurationIndex
			r.configurations.latest = snapshot.Configuration
			r.configurations.latestIndex = snapshot.ConfigurationIndex
		} else {
			configuration := decodePeers(snapshot.Peers, r.trans)
			r.configurations.committed = configuration
			r.configurations.committedIndex = snapshot.Index
			r.configurations.latest = configuration
			r.configurations.latestIndex = snapshot.Index
		}

		// Success!
		return nil
	}

	// If we had snapshots and failed to load them, its an error
	if len(snapshots) > 0 {
		return fmt.Errorf("failed to load any existing snapshots")
	}
	return nil
}

// BootstrapCluster is equivalent to non-member BootstrapCluster but can be
// called on an un-bootstrapped Raft instance after it has been created. This
// should only be called at the beginning of time for the cluster, and you
// absolutely must make sure that you call it with the same configuration on all
// the Voter servers. There is no need to bootstrap Nonvoter and Staging
// servers.
// BootstrapCluster 和包方法类似，后者可以无需Raft实例调用。
// 和之前一样，需确保在集群开始时使用相同的配置调用。
func (r *Raft) BootstrapCluster(configuration Configuration) Future {
	bootstrapReq := &bootstrapFuture{}
	bootstrapReq.init()
	bootstrapReq.configuration = configuration
	select {
	case <-r.shutdownCh:
		return errorFuture{ErrRaftShutdown}
	case r.bootstrapCh <- bootstrapReq:
		return bootstrapReq
	}
}

// Leader is used to return the current leader of the cluster.
// It may return empty string if there is no current leader
// or the leader is unknown.
// 返回当前集群的Leader，可能返回空字符
// TODO: 为什么要加锁？
func (r *Raft) Leader() ServerAddress {
	r.leaderLock.RLock()
	leader := r.leader
	r.leaderLock.RUnlock()
	return leader
}

// Apply is used to apply a command to the FSM in a highly consistent
// manner. This returns a future that can be used to wait on the application.
// An optional timeout can be provided to limit the amount of time we wait
// for the command to be started. This must be run on the leader or it
// will fail.
// 准备应用
// 强一致性向FSM应用command，返回用于等待程序的future。
// 可选参数timeout，可以提供命令执行开始的延迟时间。必须在leader上执行。
func (r *Raft) Apply(cmd []byte, timeout time.Duration) ApplyFuture {
	metrics.IncrCounter([]string{"raft", "apply"}, 1)
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}

	// Create a log future, no index or term yet
	// 创建future日志
	logFuture := &logFuture{
		log: Log{
			Type: LogCommand,
			Data: cmd,
		},
	}
	// 初始化错误通道
	logFuture.init()

	select {
	case <-timer:
		return errorFuture{ErrEnqueueTimeout}
	case <-r.shutdownCh:
		return errorFuture{ErrRaftShutdown}
	case r.applyCh <- logFuture:
		return logFuture
	}
}

// Barrier is used to issue a command that blocks until all preceeding
// operations have been applied to the FSM. It can be used to ensure the
// FSM reflects all queued writes. An optional timeout can be provided to
// limit the amount of time we wait for the command to be started. This
// must be run on the leader or it will fail.
// Barrier 用于阻塞知道直到之前所有的操作都被应用到FSM。确保FSM被顺序写。
// 可选参数timeout，可以提供命令执行开始的延迟时间。必须在leader上执行。
func (r *Raft) Barrier(timeout time.Duration) Future {
	metrics.IncrCounter([]string{"raft", "barrier"}, 1)
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}

	// Create a log future, no index or term yet
	logFuture := &logFuture{
		log: Log{
			Type: LogBarrier,
		},
	}
	logFuture.init()

	select {
	case <-timer:
		return errorFuture{ErrEnqueueTimeout}
	case <-r.shutdownCh:
		return errorFuture{ErrRaftShutdown}
	case r.applyCh <- logFuture:
		return logFuture
	}
}

// VerifyLeader is used to ensure the current node is still
// the leader. This can be done to prevent stale reads when a
// new leader has potentially been elected.
// VerifyLeader 确保当前节点是Leader。可以防止新Leader产生时读取到旧的日志。
func (r *Raft) VerifyLeader() Future {
	metrics.IncrCounter([]string{"raft", "verify_leader"}, 1)
	verifyFuture := &verifyFuture{}
	verifyFuture.init()
	select {
	case <-r.shutdownCh:
		return errorFuture{ErrRaftShutdown}
	case r.verifyCh <- verifyFuture:
		return verifyFuture
	}
}

// GetConfiguration returns the latest configuration and its associated index
// currently in use. This may not yet be committed. This must not be called on
// the main thread (which can access the information directly).
func (r *Raft) GetConfiguration() ConfigurationFuture {
	configReq := &configurationsFuture{}
	configReq.init()
	select {
	case <-r.shutdownCh:
		configReq.respond(ErrRaftShutdown)
		return configReq
	case r.configurationsCh <- configReq:
		return configReq
	}
}

// AddPeer (deprecated) is used to add a new peer into the cluster. This must be
// run on the leader or it will fail. Use AddVoter/AddNonvoter instead.
func (r *Raft) AddPeer(peer ServerAddress) Future {
	if r.protocolVersion > 2 {
		return errorFuture{ErrUnsupportedProtocol}
	}

	return r.requestConfigChange(configurationChangeRequest{
		command:       AddStaging,
		serverID:      ServerID(peer),
		serverAddress: peer,
		prevIndex:     0,
	}, 0)
}

// RemovePeer (deprecated) is used to remove a peer from the cluster. If the
// current leader is being removed, it will cause a new election
// to occur. This must be run on the leader or it will fail.
// Use RemoveServer instead.
func (r *Raft) RemovePeer(peer ServerAddress) Future {
	if r.protocolVersion > 2 {
		return errorFuture{ErrUnsupportedProtocol}
	}

	return r.requestConfigChange(configurationChangeRequest{
		command:   RemoveServer,
		serverID:  ServerID(peer),
		prevIndex: 0,
	}, 0)
}

// AddVoter will add the given server to the cluster as a staging server. If the
// server is already in the cluster as a voter, this updates the server's address.
// This must be run on the leader or it will fail. The leader will promote the
// staging server to a voter once that server is ready. If nonzero, prevIndex is
// the index of the only configuration upon which this change may be applied; if
// another configuration entry has been added in the meantime, this request will
// fail. If nonzero, timeout is how long this server should wait before the
// configuration change log entry is appended.
// AddVoter 给集群添加服务节点。如果已经存在更新服务的地址。必须在Leader上执行。
// 一旦被添加的服务准备好了，Leader 会将其推为为选民。
// prevIndex 非0，配置序号大于他时才可被应用。如果其他配置被同时添加，这个请求会失败。
func (r *Raft) AddVoter(id ServerID, address ServerAddress, prevIndex uint64, timeout time.Duration) IndexFuture {
	if r.protocolVersion < 2 {
		return errorFuture{ErrUnsupportedProtocol}
	}

	return r.requestConfigChange(configurationChangeRequest{
		command:       AddStaging,
		serverID:      id,
		serverAddress: address,
		prevIndex:     prevIndex,
	}, timeout)
}

// AddNonvoter will add the given server to the cluster but won't assign it a
// vote. The server will receive log entries, but it won't participate in
// elections or log entry commitment. If the server is already in the cluster,
// this updates the server's address. This must be run on the leader or it will
// fail. For prevIndex and timeout, see AddVoter.
// 同上，但是不推为选民。接收日志，但是不参加选举，也不进行日志提交。
func (r *Raft) AddNonvoter(id ServerID, address ServerAddress, prevIndex uint64, timeout time.Duration) IndexFuture {
	if r.protocolVersion < 3 {
		return errorFuture{ErrUnsupportedProtocol}
	}

	return r.requestConfigChange(configurationChangeRequest{
		command:       AddNonvoter,
		serverID:      id,
		serverAddress: address,
		prevIndex:     prevIndex,
	}, timeout)
}

// RemoveServer will remove the given server from the cluster. If the current
// leader is being removed, it will cause a new election to occur. This must be
// run on the leader or it will fail. For prevIndex and timeout, see AddVoter.
// 从集群移除服务，如果是当前Leader被移除，则触发新一轮的选举。
func (r *Raft) RemoveServer(id ServerID, prevIndex uint64, timeout time.Duration) IndexFuture {
	if r.protocolVersion < 2 {
		return errorFuture{ErrUnsupportedProtocol}
	}

	return r.requestConfigChange(configurationChangeRequest{
		command:   RemoveServer,
		serverID:  id,
		prevIndex: prevIndex,
	}, timeout)
}

// DemoteVoter will take away a server's vote, if it has one. If present, the
// server will continue to receive log entries, but it won't participate in
// elections or log entry commitment. If the server is not in the cluster, this
// does nothing. This must be run on the leader or it will fail. For prevIndex
// and timeout, see AddVoter.
// 取走选民的选票，取走后，该服务接收日志，但是不进行选举，也不进行日志提交。
// 如果服务不在集群中，则不进行任何操作。
func (r *Raft) DemoteVoter(id ServerID, prevIndex uint64, timeout time.Duration) IndexFuture {
	if r.protocolVersion < 3 {
		return errorFuture{ErrUnsupportedProtocol}
	}

	return r.requestConfigChange(configurationChangeRequest{
		command:   DemoteVoter,
		serverID:  id,
		prevIndex: prevIndex,
	}, timeout)
}

// Shutdown is used to stop the Raft background routines.
// This is not a graceful operation. Provides a future that
// can be used to block until all background routines have exited.
// 停止Raft的后台 routines。阻塞住知道后台routines全部停止。
func (r *Raft) Shutdown() Future {
	r.shutdownLock.Lock()
	defer r.shutdownLock.Unlock()

	if !r.shutdown {
		close(r.shutdownCh)
		r.shutdown = true
		r.setState(Shutdown)
		return &shutdownFuture{r}
	}

	// avoid closing transport twice
	return &shutdownFuture{nil}
}

// Snapshot is used to manually force Raft to take a snapshot. Returns a future
// that can be used to block until complete, and that contains a function that
// can be used to open the snapshot.
// 手动产生快照。
func (r *Raft) Snapshot() SnapshotFuture {
	future := &userSnapshotFuture{}
	future.init()
	select {
	case r.userSnapshotCh <- future:
		return future
	case <-r.shutdownCh:
		future.respond(ErrRaftShutdown)
		return future
	}
}

// Restore is used to manually force Raft to consume an external snapshot, such
// as if restoring from a backup. We will use the current Raft configuration,
// not the one from the snapshot, so that we can restore into a new cluster. We
// will also use the higher of the index of the snapshot, or the current index,
// and then add 1 to that, so we force a new state with a hole in the Raft log,
// so that the snapshot will be sent to followers and used for any new joiners.
// This can only be run on the leader, and blocks until the restore is complete
// or an error occurs.
// 手动使Raft从快照恢复，从备份恢复。我们使用当前的Raft配置，而不是快照中的， 这样我们可以恢复
// 快照到这个新的集群。我们从快照序号和当前序号中选择最大值，然后加1，这样在Raft日志上会存在有洞的
// 状态，这样快照可以被发送给followers或者新的成员。
//
// WARNING! This operation has the leader take on the state of the snapshot and
// then sets itself up so that it replicates that to its followers though the
// install snapshot process. This involves a potentially dangerous period where
// the leader commits ahead of its followers, so should only be used for disaster
// recovery into a fresh cluster, and should not be used in normal operations.
// 警告！这个操作leader会通过快照进程接管快照的状态，同时复制给followers。这意味着存在一个leader的
// commits超前于 followers 的危险期，所以这个操作只可用在故障恢复至新集群，不要在正常操作中使用。
func (r *Raft) Restore(meta *SnapshotMeta, reader io.Reader, timeout time.Duration) error {
	metrics.IncrCounter([]string{"raft", "restore"}, 1)
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}

	// Perform the restore.
	restore := &userRestoreFuture{
		meta:   meta,
		reader: reader,
	}
	restore.init()
	select {
	case <-timer:
		return ErrEnqueueTimeout
	case <-r.shutdownCh:
		return ErrRaftShutdown
	case r.userRestoreCh <- restore:
		// If the restore is ingested then wait for it to complete.
		if err := restore.Error(); err != nil {
			return err
		}
	}

	// Apply a no-op log entry. Waiting for this allows us to wait until the
	// followers have gotten the restore and replicated at least this new
	// entry, which shows that we've also faulted and installed the
	// snapshot with the contents of the restore.
	// 应用 no-op 日志，等待followers取到恢复同时复制了最少一条心的日志，
	// TODO: 没看懂
	noop := &logFuture{
		log: Log{
			Type: LogNoop,
		},
	}
	noop.init()
	select {
	case <-timer:
		return ErrEnqueueTimeout
	case <-r.shutdownCh:
		return ErrRaftShutdown
	case r.applyCh <- noop:
		return noop.Error()
	}
}

// State is used to return the current raft state.
func (r *Raft) State() RaftState {
	return r.getState()
}

// LeaderCh is used to get a channel which delivers signals on
// acquiring or losing leadership. It sends true if we become
// the leader, and false if we lose it. The channel is not buffered,
// and does not block on writes.
func (r *Raft) LeaderCh() <-chan bool {
	return r.leaderCh
}

// String returns a string representation of this Raft node.
func (r *Raft) String() string {
	return fmt.Sprintf("Node at %s [%v]", r.localAddr, r.getState())
}

// LastContact returns the time of last contact by a leader.
// This only makes sense if we are currently a follower.
// 最后和leader联系的时间，follower时才有意义。
func (r *Raft) LastContact() time.Time {
	r.lastContactLock.RLock()
	last := r.lastContact
	r.lastContactLock.RUnlock()
	return last
}

// Stats is used to return a map of various internal stats. This
// should only be used for informative purposes or debugging.
// 返回所有的状态。只可以在获取信息或调试时使用。
//
// Keys are: "state", "term", "last_log_index", "last_log_term",
// "commit_index", "applied_index", "fsm_pending",
// "last_snapshot_index", "last_snapshot_term",
// "latest_configuration", "last_contact", and "num_peers".
//
// The value of "state" is a numerical value representing a
// RaftState const.
//
// The value of "latest_configuration" is a string which contains
// the id of each server, its suffrage status, and its address.
//
// The value of "last_contact" is either "never" if there
// has been no contact with a leader, "0" if the node is in the
// leader state, or the time since last contact with a leader
// formatted as a string.
//
// The value of "num_peers" is the number of other voting servers in the
// cluster, not including this node. If this node isn't part of the
// configuration then this will be "0".
//
// All other values are uint64s, formatted as strings.
func (r *Raft) Stats() map[string]string {
	toString := func(v uint64) string {
		return strconv.FormatUint(v, 10)
	}
	lastLogIndex, lastLogTerm := r.getLastLog()
	lastSnapIndex, lastSnapTerm := r.getLastSnapshot()
	s := map[string]string{
		"state":                r.getState().String(),
		"term":                 toString(r.getCurrentTerm()),
		"last_log_index":       toString(lastLogIndex),
		"last_log_term":        toString(lastLogTerm),
		"commit_index":         toString(r.getCommitIndex()),
		"applied_index":        toString(r.getLastApplied()),
		"fsm_pending":          toString(uint64(len(r.fsmMutateCh))),
		"last_snapshot_index":  toString(lastSnapIndex),
		"last_snapshot_term":   toString(lastSnapTerm),
		"protocol_version":     toString(uint64(r.protocolVersion)),
		"protocol_version_min": toString(uint64(ProtocolVersionMin)),
		"protocol_version_max": toString(uint64(ProtocolVersionMax)),
		"snapshot_version_min": toString(uint64(SnapshotVersionMin)),
		"snapshot_version_max": toString(uint64(SnapshotVersionMax)),
	}

	future := r.GetConfiguration()
	if err := future.Error(); err != nil {
		r.logger.Printf("[WARN] raft: could not get configuration for Stats: %v", err)
	} else {
		configuration := future.Configuration()
		s["latest_configuration_index"] = toString(future.Index())
		s["latest_configuration"] = fmt.Sprintf("%+v", configuration.Servers)

		// This is a legacy metric that we've seen people use in the wild.
		hasUs := false
		numPeers := 0
		for _, server := range configuration.Servers {
			if server.Suffrage == Voter {
				if server.ID == r.localID {
					hasUs = true
				} else {
					numPeers++
				}
			}
		}
		if !hasUs {
			numPeers = 0
		}
		s["num_peers"] = toString(uint64(numPeers))
	}

	last := r.LastContact()
	if r.getState() == Leader {
		s["last_contact"] = "0"
	} else if last.IsZero() {
		s["last_contact"] = "never"
	} else {
		s["last_contact"] = fmt.Sprintf("%v", time.Now().Sub(last))
	}
	return s
}

// LastIndex returns the last index in stable storage,
// either from the last log or from the last snapshot.
func (r *Raft) LastIndex() uint64 {
	return r.getLastIndex()
}

// AppliedIndex returns the last index applied to the FSM. This is generally
// lagging behind the last index, especially for indexes that are persisted but
// have not yet been considered committed by the leader. NOTE - this reflects
// the last index that was sent to the application's FSM over the apply channel
// but DOES NOT mean that the application's FSM has yet consumed it and applied
// it to its internal state. Thus, the application's state may lag behind this
// index.
// 返回最后被应用到FSM的序号，必然是滞后于最后序号。TODO：先持久化再确认？
func (r *Raft) AppliedIndex() uint64 {
	return r.getLastApplied()
}
