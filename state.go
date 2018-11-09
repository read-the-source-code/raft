package raft

import (
	"sync"
	"sync/atomic"
)

// RaftState captures the state of a Raft node: Follower, Candidate, Leader,
// or Shutdown.
// RaftState 表明 Raft 节点的3中工作状态：Follower(从)、Candidate(候选)、Leader(主), 以及关闭状态。
type RaftState uint32

const (
	// Follower is the initial state of a Raft node.
	// Follower 是节点的初始状态。
	Follower RaftState = iota

	// Candidate is one of the valid states of a Raft node.
	// Candidate 状态
	Candidate

	// Leader is one of the valid states of a Raft node.
	// Leader 状态
	Leader

	// Shutdown is the terminal state of a Raft node.
	// 关闭是终止状态
	Shutdown
)

// String 返回状态的字符串
func (s RaftState) String() string {
	switch s {
	case Follower:
		return "Follower"
	case Candidate:
		return "Candidate"
	case Leader:
		return "Leader"
	case Shutdown:
		return "Shutdown"
	default:
		return "Unknown"
	}
}

// raftState is used to maintain various state variables
// and provides an interface to set/get the variables in a
// thread safe manner.
// raftState 用于保存各种状态的各种信息，同时提供线程安全的信息操作接口 set/get 。
type raftState struct {
	// currentTerm commitIndex, lastApplied,  must be kept at the top of
	// the struct so they're 64 bit aligned which is a requirement for
	// atomic ops on 32 bit platforms.
	// currentTerm、commitIndex、lastApplied 必须放在结构体的顶部，
	// 这样他们可以满足在32位平台上原子操作的64位对齐。
	// 为什么在32位上的原子操作需要64位对齐？找到原因是 atomic 包的bug，Go的文档中有说明。
	// `sync/atomic` expects the first word in an allocated struct to be 64-bit
	// aligned on both ARM and x86-32. See https://goo.gl/zW7dgq for more details.

	// The current term, cache of StableStore
	// 当前任期，StableStore的缓存
	currentTerm uint64

	// Highest committed log entry
	// 最大提交日志
	commitIndex uint64

	// Last applied log to the FSM
	// 最后应用给FSM的日志
	lastApplied uint64

	// protects 4 next fields
	// 线程安全提供给下面的4个字段
	// TODO: 为什么uint64需要锁，W/R？
	lastLock sync.Mutex

	// Cache the latest snapshot index/term
	// 最后快照的 序号/任期 缓存
	lastSnapshotIndex uint64
	lastSnapshotTerm  uint64

	// Cache the latest log from LogStore
	// 最后一条日志的 序号/任期 缓存，来自LogStore
	lastLogIndex uint64
	lastLogTerm  uint64

	// Tracks running goroutines
	// 运行的 goroutines 跟踪
	routinesGroup sync.WaitGroup

	// The current state
	// 当前状态
	state RaftState
}

// 获取当前状态
func (r *raftState) getState() RaftState {
	// 取得状态值的地址，返回通过原子操作读取地址的值(类型转换)
	// TODO: 为什么需要通过地址来操作？
	stateAddr := (*uint32)(&r.state)
	return RaftState(atomic.LoadUint32(stateAddr))
}

// 设置当前状态
func (r *raftState) setState(s RaftState) {
	// 取得状态的地址，使用atomic设置地址的值
	stateAddr := (*uint32)(&r.state)
	atomic.StoreUint32(stateAddr, uint32(s))
}

// 获取当前任期
func (r *raftState) getCurrentTerm() uint64 {
	return atomic.LoadUint64(&r.currentTerm)
}

// 设置当前任期
func (r *raftState) setCurrentTerm(term uint64) {
	atomic.StoreUint64(&r.currentTerm, term)
}

// 获取最后日志序号及所在任期
func (r *raftState) getLastLog() (index, term uint64) {
	r.lastLock.Lock()
	index = r.lastLogIndex
	term = r.lastLogTerm
	r.lastLock.Unlock()
	return
}

// 设置最后日志的序号及所在任期
func (r *raftState) setLastLog(index, term uint64) {
	r.lastLock.Lock()
	r.lastLogIndex = index
	r.lastLogTerm = term
	r.lastLock.Unlock()
}

// 获取最后快照的序号及所在任期
func (r *raftState) getLastSnapshot() (index, term uint64) {
	r.lastLock.Lock()
	index = r.lastSnapshotIndex
	term = r.lastSnapshotTerm
	r.lastLock.Unlock()
	return
}

// 设置最后快照的序号及所在任期
func (r *raftState) setLastSnapshot(index, term uint64) {
	r.lastLock.Lock()
	r.lastSnapshotIndex = index
	r.lastSnapshotTerm = term
	r.lastLock.Unlock()
}

// TODO: 下面几个为什么需要用过atomic来处理？

// 获取提交的序号
func (r *raftState) getCommitIndex() uint64 {
	return atomic.LoadUint64(&r.commitIndex)
}

// 设置提交的序号
func (r *raftState) setCommitIndex(index uint64) {
	atomic.StoreUint64(&r.commitIndex, index)
}

// 获取最后一次应用的序号
func (r *raftState) getLastApplied() uint64 {
	return atomic.LoadUint64(&r.lastApplied)
}

// 设置最后一次应用的序号
func (r *raftState) setLastApplied(index uint64) {
	atomic.StoreUint64(&r.lastApplied, index)
}

// Start a goroutine and properly handle the race between a routine
// starting and incrementing, and exiting and decrementing.
// 启动一个 goroutine，更好地处理协程启动和关闭的竞争。
func (r *raftState) goFunc(f func()) {
	r.routinesGroup.Add(1)
	go func() {
		defer r.routinesGroup.Done()
		f()
	}()
}

func (r *raftState) waitShutdown() {
	r.routinesGroup.Wait()
}

// getLastIndex returns the last index in stable storage.
// Either from the last log or from the last snapshot.
// getLastIndex 返回落盘的最后序号，取最后日志序号与最后快照日志序号的最大值。
func (r *raftState) getLastIndex() uint64 {
	r.lastLock.Lock()
	defer r.lastLock.Unlock()
	return max(r.lastLogIndex, r.lastSnapshotIndex)
}

// getLastEntry returns the last index and term in stable storage.
// Either from the last log or from the last snapshot.
// getLastEntry 返回落盘的最后序号和任期，取最后日志序号与最后快照日志序号的最大值。
func (r *raftState) getLastEntry() (uint64, uint64) {
	r.lastLock.Lock()
	defer r.lastLock.Unlock()
	if r.lastLogIndex >= r.lastSnapshotIndex {
		return r.lastLogIndex, r.lastLogTerm
	}
	return r.lastSnapshotIndex, r.lastSnapshotTerm
}
