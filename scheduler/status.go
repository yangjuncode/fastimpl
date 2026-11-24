package scheduler

import (
	"slices"
	"sync"

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
