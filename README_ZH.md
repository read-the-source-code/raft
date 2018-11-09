raft [![Build Status](https://travis-ci.org/hashicorp/raft.png)](https://travis-ci.org/hashicorp/raft)
====

raft is a [Go](http://www.golang.org) library that manages a replicated
log and can be used with an FSM to manage replicated state machines. It
is a library for providing [consensus](http://en.wikipedia.org/wiki/Consensus_(computer_science)).

raft 是一个用来管理日志复制和通过FSM来管理复制状态机的 [Go](http://www.golang.org) 语言库。
换句话讲，raft 是用来给分布式系统开发提供一致性功能的库。

The use cases for such a library are far-reaching as replicated state
machines are a key component of many distributed systems. They enable
building Consistent, Partition Tolerant (CP) systems, with limited
fault tolerance as well.

复制状态机在大多数的分布式系统中应用广泛，给系统提供一致性、分区容忍，以及一定的容错。

## Building | 编译

If you wish to build raft you'll need Go version 1.2+ installed.

编译 raft 安装1.2+版本的Go。

Please check your installation with:

```
go version
```

## Documentation | 文档

For complete documentation, see the associated [Godoc](http://godoc.org/github.com/hashicorp/raft).

完整文档查看相关的 [Godoc](http://godoc.org/github.com/hashicorp/raft)。

To prevent complications with cgo, the primary backend `MDBStore` is in a separate repository,
called [raft-mdb](http://github.com/hashicorp/raft-mdb). That is the recommended implementation
for the `LogStore` and `StableStore`.

`MDBStore`作为存储后端，需要cgo支持，在[raft-mdb](http://github.com/hashicorp/raft-mdb)这个仓库中。
这个是推荐的`LogStore` 和 `StableStore`的实现。

A pure Go backend using [BoltDB](https://github.com/boltdb/bolt) is also available called
[raft-boltdb](https://github.com/hashicorp/raft-boltdb). It can also be used as a `LogStore`
and `StableStore`.

纯Go版的存储后端使用的是 [BoltDB](https://github.com/boltdb/bolt)在仓库[raft-boltdb](https://github.com/hashicorp/raft-boltdb)中。
她也可以作为`LogStore` 和 `StableStore`使用。

## Tagged Releases | 发布版本

As of September 2017, HashiCorp will start using tags for this library to clearly indicate
major version updates. We recommend you vendor your application's dependency on this library.

* v0.1.0 is the original stable version of the library that was in master and has been maintained
with no breaking API changes. This was in use by Consul prior to version 0.7.0.

* v1.0.0 takes the changes that were staged in the library-v2-stage-one branch. This version
manages server identities using a UUID, so introduces some breaking API changes. It also versions
the Raft protocol, and requires some special steps when interoperating with Raft servers running
older versions of the library (see the detailed comment in config.go about version compatibility).
You can reference https://github.com/hashicorp/consul/pull/2222 for an idea of what was required
to port Consul to these new interfaces.

    This version includes some new features as well, including non voting servers, a new address
    provider abstraction in the transport layer, and more resilient snapshots.

## Protocol | 协议

raft is based on ["Raft: In Search of an Understandable Consensus Algorithm"](https://ramcloud.stanford.edu/wiki/download/attachments/11370504/raft.pdf)

raft 是基于["Raft: In Search of an Understandable Consensus Algorithm"](https://ramcloud.stanford.edu/wiki/download/attachments/11370504/raft.pdf)这篇论文来实现来的。

A high level overview of the Raft protocol is described below, but for details please read the full
[Raft paper](https://ramcloud.stanford.edu/wiki/download/attachments/11370504/raft.pdf)
followed by the raft source. Any questions about the raft protocol should be sent to the
[raft-dev mailing list](https://groups.google.com/forum/#!forum/raft-dev).

下面有一个关于 Raft 协议的概要，详细了解 Raft 算法还是需要去查看论文：[Raft paper](https://ramcloud.stanford.edu/wiki/download/attachments/11370504/raft.pdf)。
有关 raft 协议的任何问题请在邮件组[raft-dev mailing list](https://groups.google.com/forum/#!forum/raft-dev)反馈。

### Protocol Description | 协议描述

Raft nodes are always in one of three states: follower, candidate or leader. All
nodes initially start out as a follower. In this state, nodes can accept log entries
from a leader and cast votes. If no entries are received for some time, nodes
self-promote to the candidate state. In the candidate state nodes request votes from
their peers. If a candidate receives a quorum of votes, then it is promoted to a leader.
The leader must accept new log entries and replicate to all the other followers.
In addition, if stale reads are not acceptable, all queries must also be performed on
the leader.

Raft 的节点只有三种状态：follower, candidate or leader之一。所有节点都是以 follower 启动。
在这个状态下，节点可以接收来自 leader 的日志以及选举请求。

Once a cluster has a leader, it is able to accept new log entries. A client can
request that a leader append a new log entry, which is an opaque binary blob to
Raft. The leader then writes the entry to durable storage and attempts to replicate
to a quorum of followers. Once the log entry is considered *committed*, it can be
*applied* to a finite state machine. The finite state machine is application specific,
and is implemented using an interface.

An obvious question relates to the unbounded nature of a replicated log. Raft provides
a mechanism by which the current state is snapshotted, and the log is compacted. Because
of the FSM abstraction, restoring the state of the FSM must result in the same state
as a replay of old logs. This allows Raft to capture the FSM state at a point in time,
and then remove all the logs that were used to reach that state. This is performed automatically
without user intervention, and prevents unbounded disk usage as well as minimizing
time spent replaying logs.

Lastly, there is the issue of updating the peer set when new servers are joining
or existing servers are leaving. As long as a quorum of nodes is available, this
is not an issue as Raft provides mechanisms to dynamically update the peer set.
If a quorum of nodes is unavailable, then this becomes a very challenging issue.
For example, suppose there are only 2 peers, A and B. The quorum size is also
2, meaning both nodes must agree to commit a log entry. If either A or B fails,
it is now impossible to reach quorum. This means the cluster is unable to add,
or remove a node, or commit any additional log entries. This results in *unavailability*.
At this point, manual intervention would be required to remove either A or B,
and to restart the remaining node in bootstrap mode.

A Raft cluster of 3 nodes can tolerate a single node failure, while a cluster
of 5 can tolerate 2 node failures. The recommended configuration is to either
run 3 or 5 raft servers. This maximizes availability without
greatly sacrificing performance.

In terms of performance, Raft is comparable to Paxos. Assuming stable leadership,
committing a log entry requires a single round trip to half of the cluster.
Thus performance is bound by disk I/O and network latency.

