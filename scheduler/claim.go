package scheduler

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/yangjuncode/fastimpl/forge"
	"github.com/yangjuncode/fastimpl/graph"
)

// 认领标记协议：issue 评论里的一行人类可读文本，可直接搜索。
// issue 本身由页面上下文给出，标记里不重复 issue 号。
const (
	claimVersion = "fastimpl-claim/v1"
	claimKindYes = "claim"
	claimKindNo  = "release"
)

// 认领相关的默认时间：发 claim 后的短等待、运行期复查间隔、
// 单次复查/释放的读取上限。
const (
	DefaultClaimWait    = time.Second
	DefaultClaimRecheck = 15 * time.Second
	ClaimReadBudget     = 10 * time.Second
)

// claimLineRE 是标记的严格单行格式：
// fastimpl-claim/v1 claim|release <instance-id> <claim-id>。
var claimLineRE = regexp.MustCompile(`^fastimpl-claim/v1 (claim|release) ([A-Za-z0-9._-]{1,128}) ([A-Za-z0-9._-]{1,128})$`)

// ErrClaimLost 表示本任务的认领已失效（更早的认领出现或自己的标记被删）。
var ErrClaimLost = errors.New("cross-machine claim lost")

// claimLine 渲染一条认领标记评论。
func claimLine(kind, instance, token string) string {
	return claimVersion + " " + kind + " " + instance + " " + token
}

// parseClaim 解析评论正文里的标记；不是标记的评论返回 ok=false。
func parseClaim(body string) (kind, instance, token string, ok bool) {
	line := strings.TrimSpace(body)
	if strings.ContainsAny(line, "\r\n") {
		return "", "", "", false
	}
	m := claimLineRE.FindStringSubmatch(line)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}

type activeClaim struct {
	commentID int64
	authorID  int64
	createdAt time.Time
	instance  string
	token     string
}

func (a activeClaim) same(b activeClaim) bool {
	return a.commentID == b.commentID && a.authorID == b.authorID &&
		a.instance == b.instance && a.token == b.token
}

type claimKey struct {
	authorID int64
	instance string
	token    string
}

type claimRecord struct {
	activeClaim
	released bool
}

// activeClaims 按服务端时间（相同时间按数值 ID）升序回放评论，返回仍然
// 有效的认领，最早出现的在前。同一 (author, instance, token) 只保留首次
// 出现的位置：release 给它立墓碑后，重复的 claim 不会复活；release 只对
// 已经出现过的同作者认领生效，因此重复 release 幂等。无关评论完全忽略。
func activeClaims(comments []forge.IssueComment) ([]activeClaim, error) {
	ordered := slices.Clone(comments)
	slices.SortFunc(ordered, func(a, b forge.IssueComment) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	var records []*claimRecord
	index := map[claimKey]*claimRecord{}
	for _, c := range ordered {
		kind, instance, token, ok := parseClaim(c.Body)
		if !ok {
			continue
		}
		if c.ID == 0 || c.AuthorID == 0 || c.CreatedAt.IsZero() {
			return nil, fmt.Errorf("claim marker (%s %s) at comment %d lacks a verifiable server id/time/author",
				kind, instance, c.ID)
		}
		key := claimKey{authorID: c.AuthorID, instance: instance, token: token}
		switch kind {
		case claimKindYes:
			if _, seen := index[key]; seen {
				continue
			}
			rec := &claimRecord{activeClaim: activeClaim{
				commentID: c.ID,
				authorID:  c.AuthorID,
				createdAt: c.CreatedAt,
				instance:  instance,
				token:     token,
			}}
			index[key] = rec
			records = append(records, rec)
		case claimKindNo:
			if rec, seen := index[key]; seen {
				rec.released = true
			}
		}
	}
	var active []activeClaim
	for _, rec := range records {
		if !rec.released {
			active = append(active, rec.activeClaim)
		}
	}
	return active, nil
}

// claimState 读一次评论并给出仲裁结果：winner 是最早的活跃认领，
// mine 是本次运行自己的认领（可能为空）。
func (s *Scheduler) claimState(ctx context.Context, key graph.Key, token string) (winner, mine *activeClaim, err error) {
	comments, err := s.forge.IssueComments(ctx, key.ProjectID, key.IID)
	if err != nil {
		return nil, nil, fmt.Errorf("read issue comments: %w", err)
	}
	active, err := activeClaims(comments)
	if err != nil {
		return nil, nil, err
	}
	if len(active) == 0 {
		return nil, nil, nil
	}
	winner = &active[0]
	for i := range active {
		if active[i].token == token && active[i].instance == s.cfg.InstanceID {
			mine = &active[i]
			break
		}
	}
	return winner, mine, nil
}

// holdsClaim 报告本次运行是否持有最早的有效认领。
func holdsClaim(winner, mine *activeClaim) bool {
	return mine != nil && (winner == nil || winner.same(*mine))
}

// boundedRead 用有界 context 读一次认领状态，避免复查卡住任务主流程。
func (s *Scheduler) boundedRead(ctx context.Context, key graph.Key, token string) (winner, mine *activeClaim, err error) {
	rctx, cancel := context.WithTimeout(ctx, ClaimReadBudget)
	defer cancel()
	return s.claimState(rctx, key, token)
}

// claimBusy 报告已存在的活跃认领（不是自己），用于告警与转人工。
func claimBusy(winner *activeClaim) error {
	if winner == nil {
		return nil
	}
	return fmt.Errorf("issue already claimed by instance %s (claim %s, comment %d); clear that comment by hand if it is stale",
		winner.instance, winner.token, winner.commentID)
}

// postMarker 在 issue 上发一条 claim/release 标记评论。
func (s *Scheduler) postMarker(ctx context.Context, key graph.Key, kind, token string) error {
	line := claimLine(kind, s.cfg.InstanceID, token)
	if err := s.forge.Note(ctx, key.ProjectID, key.IID, line); err != nil {
		return fmt.Errorf("post %s marker: %w", kind, err)
	}
	return nil
}

type claimMonitor struct {
	done     chan struct{}
	quit     chan struct{}
	stopRead context.CancelFunc
}

func (m *claimMonitor) stop() {
	if m == nil {
		return
	}
	select {
	case <-m.quit:
	default:
		close(m.quit)
	}
	m.stopRead()
	<-m.done
}

// watchClaim 每 claimRecheck 复查一次认领：看到更早的活跃认领或自己的
// 标记被删除时取消任务 context。读取暂时失败只告警，不当作已经获胜或
// 已经失锁。stop 会立刻取消正在进行的读取，不让调用方白等一次超时。
func (s *Scheduler) watchClaim(ctx context.Context, key graph.Key, token string, cancel context.CancelCauseFunc) *claimMonitor {
	// readCtx 在 stop 时取消：正在进行的读取随之返回，goroutine 立刻退出。
	readCtx, stopRead := context.WithCancel(ctx)
	m := &claimMonitor{done: make(chan struct{}), quit: make(chan struct{}), stopRead: stopRead}
	go func() {
		defer close(m.done)
		ticker := time.NewTicker(s.claimRecheck)
		defer ticker.Stop()
		for {
			select {
			case <-readCtx.Done():
				return
			case <-m.quit:
				stopRead()
				return
			case <-ticker.C:
			}
			winner, mine, err := s.boundedRead(readCtx, key, token)
			if err != nil {
				s.log.Warn("recheck claim failed; keeping the claim", "key", key, "err", err)
				continue
			}
			switch {
			case mine == nil:
				cancel(fmt.Errorf("%w: our claim marker is gone from issue comments", ErrClaimLost))
				return
			case !holdsClaim(winner, mine):
				cancel(fmt.Errorf("%w: instance %s claimed earlier (claim %s, comment %d)",
					ErrClaimLost, winner.instance, winner.token, winner.commentID))
				return
			}
		}
	}()
	return m
}
