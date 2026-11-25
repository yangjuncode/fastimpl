package scheduler

import (
	"slices"
	"sync"
	"time"

	"github.com/yangjuncode/fastimpl/graph"
)

// runtimeStore 保存状态视图要的、图里没有的运行数据：终端地址与
// 终端名、注意原因、已接受提交。它有自己的锁，只在极短的读写里
// 持有——绝不在持锁时做 IO，也不与 graph 的锁嵌套。
type runtimeStore struct {
	mu    sync.Mutex
	tasks map[graph.Key]TaskRuntime
}

// update 在锁内改一个任务的条目，不存在时先建。
func (s *runtimeStore) update(k graph.Key, fn func(*TaskRuntime)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tasks == nil {
		s.tasks = make(map[graph.Key]TaskRuntime)
	}
	r, ok := s.tasks[k]
	if !ok {
		r = TaskRuntime{Key: k}
	}
	fn(&r)
	s.tasks[k] = r
}

// setAddr 公开一次运行中的终端；label 是终端名。
func (s *runtimeStore) setAddr(k graph.Key, addr, label string, integration bool) {
	s.update(k, func(r *TaskRuntime) {
		r.Addr = addr
		r.Label = label
		r.Integration = integration
	})
}

// clearAddr 在确认终端已停止后清除地址；名字留在原位，视图仍能
// 说明这一项曾经是什么。
func (s *runtimeStore) clearAddr(k graph.Key) {
	s.update(k, func(r *TaskRuntime) {
		r.Addr = ""
	})
}

// start 记录本轮工作开始使用终端的时刻，并清掉上一轮的结束时间。
// retained terminal 被新任务复用时也调用它：工作时长属于本轮任务，
// 不把已完成轮次与闲置保留时间算进去。
func (s *runtimeStore) start(k graph.Key, at time.Time) {
	s.update(k, func(r *TaskRuntime) {
		r.StartedAt = at
		r.FinishedAt = time.Time{}
	})
}

// finish 记录工作结局确定的时刻。只有启动过的终端才有结束时间；
// 重复调用保留第一次结局，避免后续状态合并把时间往后推。
func (s *runtimeStore) finish(k graph.Key, at time.Time) {
	s.update(k, func(r *TaskRuntime) {
		if !r.StartedAt.IsZero() && r.FinishedAt.IsZero() {
			r.FinishedAt = at
		}
	})
}

// setTimes 直接恢复一段已经发生的终端工作时间；只用于重启后接管
// retained terminal，不参与 active worker 的计时重置语义。
func (s *runtimeStore) setTimes(k graph.Key, started, finished time.Time) {
	s.update(k, func(r *TaskRuntime) {
		r.StartedAt = started
		r.FinishedAt = finished
	})
}

// times 读取当前工作时间，供 retained metadata 持久化。
func (s *runtimeStore) times(k graph.Key) (time.Time, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.tasks[k]
	return r.StartedAt, r.FinishedAt
}

// setAttention 记录进入 needs attention 的原因；空串表示清除。
func (s *runtimeStore) setAttention(k graph.Key, reason string) {
	s.update(k, func(r *TaskRuntime) { r.Attention = reason })
}

// setAccepted 记录最近一次被接受的提交。
func (s *runtimeStore) setAccepted(k graph.Key, commit string) {
	s.update(k, func(r *TaskRuntime) { r.Accepted = commit })
}

// snapshot 返回按 key 排序的副本。
func (s *runtimeStore) snapshot() []TaskRuntime {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TaskRuntime, 0, len(s.tasks))
	for _, r := range s.tasks {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b TaskRuntime) int {
		if a.Key.ProjectID != b.Key.ProjectID {
			return a.Key.ProjectID - b.Key.ProjectID
		}
		if a.Key.IID != b.Key.IID {
			return a.Key.IID - b.Key.IID
		}
		return 0
	})
	return out
}
