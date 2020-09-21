package raft

import (
	"fmt"
	"io"
	"time"

	"github.com/armon/go-metrics"
)

// SnapshotMeta is for metadata of a snapshot.
type SnapshotMeta struct {
	// Version is the version number of the snapshot metadata. This does not cover
	// the application's data in the snapshot, that should be versioned
	// separately.
	Version SnapshotVersion

	// ID is opaque to the store, and is used for opening.
	ID string

	// Index and Term store when the snapshot was taken.
	Index uint64
	Term  uint64

	// Peers is deprecated and used to support version 0 snapshots, but will
	// be populated in version 1 snapshots as well to help with upgrades.
	Peers []byte

	// Configuration and ConfigurationIndex are present in version 1
	// snapshots and later.
	Configuration      Configuration
	ConfigurationIndex uint64

	// Size is the size of the snapshot in bytes.
	Size int64
}

// SnapshotStore interface is used to allow for flexible implementations
// of snapshot storage and retrieval. For example, a client could implement
// a shared state store such as S3, allowing new nodes to restore snapshots
// without streaming from the leader.
// SnapshotStore 快照存储接口，提供快照存储和检索 Create/List/Open。
// 例如，可以实现S3作为存储，从而不从leader来恢复快照。
type SnapshotStore interface {
	// Create is used to begin a snapshot at a given index and term, and with
	// the given committed configuration. The version parameter controls
	// which snapshot version to create.
	// 创建快照，SnapshotVersion，当前索引，任期，配置等参数
	Create(version SnapshotVersion, index, term uint64, configuration Configuration,
		configurationIndex uint64, trans Transport) (SnapshotSink, error)

	// List is used to list the available snapshots in the store.
	// It should return then in descending order, with the highest index first.
	// 列出存储中可用的快照，降序，最新的在第一个
	List() ([]*SnapshotMeta, error)

	// Open takes a snapshot ID and provides a ReadCloser. Once close is
	// called it is assumed the snapshot is no longer needed.
	// 根据快照ID，打开快照返回 io.ReadCloser,一旦被关闭，表示该快照不再需要
	Open(id string) (*SnapshotMeta, io.ReadCloser, error)
}

// SnapshotSink is returned by StartSnapshot. The FSM will Write state
// to the sink and call Close on completion. On error, Cancel will be invoked.
// SnapshotSink 创建快照时返回。FSM会写状态到这里，并在完成时Close，在出错时
// Cancel 会被调用
type SnapshotSink interface {
	io.WriteCloser
	ID() string
	Cancel() error
}

// runSnapshots is a long running goroutine used to manage taking
// new snapshots of the FSM. It runs in parallel to the FSM and
// main goroutines, so that snapshots do not block normal operation.
// runSnapshots 启动常驻 goroutine，管理FSM的新快照。启动一个新 goroutine
// 来并发处理。
func (r *Raft) runSnapshots() {
	for {
		// 循环执行，直到程序退出

		select {
		case <-randomTimeout(r.conf.SnapshotInterval):
			// 定时执行，时间在 interval - 2interval 之间随机，错开

			// Check if we should snapshot
			// 先检查是否需要生成快照
			if !r.shouldSnapshot() {
				continue
			}

			// Trigger a snapshot
			// 触发生成快照
			if _, err := r.takeSnapshot(); err != nil {
				r.logger.Printf("[ERR] raft: Failed to take snapshot: %v", err)
			}

		case future := <-r.userSnapshotCh:
			// 用户主动触发快照的channel，不判断时候需要，立即执行生成快照
			// User-triggered, run immediately
			id, err := r.takeSnapshot()
			if err != nil {
				r.logger.Printf("[ERR] raft: Failed to take snapshot: %v", err)
			} else {
				// 产生一个回调给 快照触发，也可以直接返回快照id，主动打开
				future.opener = func() (*SnapshotMeta, io.ReadCloser, error) {
					return r.snapshots.Open(id)
				}
			}
			future.respond(err)

		case <-r.shutdownCh:
			// 关闭时，主动退出循环
			return
		}
	}
}

// shouldSnapshot checks if we meet the conditions to take
// a new snapshot.
// shouldSnapshot 检查是否生成快照，条件为：最新快照与日志中最后一条
// 之差大于配置中的阈值
func (r *Raft) shouldSnapshot() bool {
	// Check the last snapshot index
	lastSnap, _ := r.getLastSnapshot()

	// Check the last log index
	lastIdx, err := r.logs.LastIndex()
	if err != nil {
		r.logger.Printf("[ERR] raft: Failed to get last log index: %v", err)
		return false
	}

	// Compare the delta to the threshold
	delta := lastIdx - lastSnap
	return delta >= r.conf.SnapshotThreshold
}

// takeSnapshot is used to take a new snapshot. This must only be called from
// the snapshot thread, never the main thread. This returns the ID of the new
// snapshot, along with an error.
// takeSnapshot 产生一个新快照。只能从快照线程调用，不能在主线程中。返回新快照的ID。
func (r *Raft) takeSnapshot() (string, error) {
	// metrics 指标，time.Now 立即被计算
	defer metrics.MeasureSince([]string{"raft", "snapshot", "takeSnapshot"}, time.Now())

	// Create a request for the FSM to perform a snapshot.
	// 创建FSM执行快照的请求
	snapReq := &reqSnapshotFuture{}
	snapReq.init()

	// Wait for dispatch or shutdown.
	// 等待调度或者程序退出
	select {
	case r.fsmSnapshotCh <- snapReq:
	case <-r.shutdownCh:
		return "", ErrRaftShutdown
	}

	// 等待调度和等待响应简洁但不清晰

	// Wait until we get a response
	// 等待响应
	if err := snapReq.Error(); err != nil {
		if err != ErrNothingNewToSnapshot {
			err = fmt.Errorf("failed to start snapshot: %v", err)
		}
		return "", err
	}
	defer snapReq.snapshot.Release() // 最后释放/关闭

	// Make a request for the configurations and extract the committed info.
	// We have to use the future here to safely get this information since
	// it is owned by the main thread.
	// 创建配置请求
	// 并提取提交的信息
	configReq := &configurationsFuture{}
	configReq.init()
	select {
	case r.configurationsCh <- configReq: // 等待任务调度执行
	case <-r.shutdownCh:
		return "", ErrRaftShutdown
	}
	// 阻塞获取结果
	if err := configReq.Error(); err != nil {
		return "", err
	}
	committed := configReq.configurations.committed
	committedIndex := configReq.configurations.committedIndex

	// We don't support snapshots while there's a config change outstanding
	// since the snapshot doesn't have a means to represent this state. This
	// is a little weird because we need the FSM to apply an index that's
	// past the configuration change, even though the FSM itself doesn't see
	// the configuration changes. It should be ok in practice with normal
	// application traffic flowing through the FSM. If there's none of that
	// then it's not crucial that we snapshot, since there's not much going
	// on Raft-wise.
	// 当存在配置未提交时，不能产生快照，因为快照无法表示当前的状态
	// 快照index < config 提交的 index
	if snapReq.index < committedIndex {
		return "", fmt.Errorf("cannot take snapshot now, wait until the configuration entry at %v has been applied (have applied %v)",
			committedIndex, snapReq.index)
	}

	// Create a new snapshot.
	// 创建一个新的快照
	r.logger.Printf("[INFO] raft: Starting snapshot up to %d", snapReq.index)
	start := time.Now()
	version := getSnapshotVersion(r.protocolVersion) // 目前固定返回 1
	// 创建新快照对象: 版本, 快照索引, 任期, 提交的配置, 提交索引, 传输层
	sink, err := r.snapshots.Create(version, snapReq.index, snapReq.term, committed, committedIndex, r.trans)
	if err != nil {
		return "", fmt.Errorf("failed to create snapshot: %v", err)
	}
	// mertrics 创建结束，报告计时
	metrics.MeasureSince([]string{"raft", "snapshot", "create"}, start)

	// Try to persist the snapshot.
	// 尝试持久化快照
	start = time.Now()
	if err := snapReq.snapshot.Persist(sink); err != nil {
		sink.Cancel()
		return "", fmt.Errorf("failed to persist snapshot: %v", err)
	}
	// mertrics 持久化结束，报告计时
	metrics.MeasureSince([]string{"raft", "snapshot", "persist"}, start)

	// Close and check for error.
	// 关闭并检查结果
	if err := sink.Close(); err != nil {
		return "", fmt.Errorf("failed to close snapshot: %v", err)
	}

	// Update the last stable snapshot info.
	// 更新最后一个快照信息
	r.setLastSnapshot(snapReq.index, snapReq.term)

	// Compact the logs.
	// 缩减日志
	if err := r.compactLogs(snapReq.index); err != nil {
		return "", err
	}

	r.logger.Printf("[INFO] raft: Snapshot to %d complete", snapReq.index)
	return sink.ID(), nil
}

// compactLogs takes the last inclusive index of a snapshot
// and trims the logs that are no longer needed.
// compactLogs 拿到最后一个快照索引并裁剪掉不需要的日志
func (r *Raft) compactLogs(snapIdx uint64) error {
	defer metrics.MeasureSince([]string{"raft", "compactLogs"}, time.Now())
	// Determine log ranges to compact
	minLog, err := r.logs.FirstIndex()
	if err != nil {
		return fmt.Errorf("failed to get first log index: %v", err)
	}

	// Check if we have enough logs to truncate
	// 判断是否有足够的日志来做截断
	lastLogIdx, _ := r.getLastLog()
	if lastLogIdx <= r.conf.TrailingLogs {
		return nil
	}

	// Truncate up to the end of the snapshot, or `TrailingLogs`
	// back from the head, which ever is further back. This ensures
	// at least `TrailingLogs` entries, but does not allow logs
	// after the snapshot to be removed.
	// 裁剪日志，全部或者，留下 TrailingLogs 部分
	maxLog := min(snapIdx, lastLogIdx-r.conf.TrailingLogs)

	// Log this
	r.logger.Printf("[INFO] raft: Compacting logs from %d to %d", minLog, maxLog)

	// Compact the logs
	// 删掉范围内的日志
	if err := r.logs.DeleteRange(minLog, maxLog); err != nil {
		return fmt.Errorf("log compaction failed: %v", err)
	}
	return nil
}
