package opschat

import (
	"context"
	"time"
)

func (s *Service) publicSession(c Session) Session {
	c.Title = s.scrub(c.Title, 320)
	c.Draft = s.scrub(c.Draft, 8<<10)
	return c
}

// sessionUsage 在**既有 queryer**(同事务/同连接;SQLite 下另开连接会锁死)
// 上取会话最近一条带实测 usage 的 assistant 回复并折算占用(最多回看 5 条;
// 无实测时返回 nil,前端隐藏占用条)。
func (s *Service) sessionUsage(ctx context.Context, q querier, sessionID string) *ContextUsage {
	queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := q.QueryContext(queryCtx,
		`SELECT body FROM ops_chat_entries WHERE session_id=? AND kind='assistant' AND context_allowed=1 ORDER BY seq DESC LIMIT 5`, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var usage *AgentUsage
	for rows.Next() {
		var b []byte
		if rows.Scan(&b) != nil {
			return nil
		}
		var body struct {
			Usage *AgentUsage
		}
		if s.open(b, &body) != nil {
			return nil
		}
		if body.Usage != nil && body.Usage.InputTokens > 0 {
			usage = body.Usage
			break
		}
	}
	if usage == nil {
		return nil
	}
	window := s.policy().WindowTokens
	used := usage.InputTokens + usage.OutputTokens
	percent := 0
	if window > 0 {
		percent = (used*100 + window - 1) / window
	}
	return &ContextUsage{UsedTokens: used, WindowTokens: window, Percent: percent}
}

func (s *Service) publicEntry(e Entry) Entry {
	e.Text = s.masker.ScrubTruncated(e.Text)
	if e.TargetNames != nil {
		names := map[string]string{}
		for alias, name := range e.TargetNames {
			if validTargetAlias(alias) && s.masker.Scrub(alias) == alias {
				if name = s.scrub(name, 1024); name != "" {
					names[alias] = name
				}
			}
		}
		e.TargetNames = names
	}
	if e.Summary != nil {
		copy := &RunSummary{Servers: append([]ServerSummary(nil), e.Summary.Servers...)}
		for i := range copy.Servers {
			server := &copy.Servers[i]
			server.ServerName = s.scrub(server.ServerName, 256)
			server.Facts = append([]CallFact(nil), server.Facts...)
			for j := range server.Facts {
				f := &server.Facts[j]
				v := s.publicCall(Call{Output: f.Excerpt, Error: f.Error, Resources: f.Resources})
				f.Excerpt, f.Error, f.Resources = v.Output, v.Error, v.Resources
				f.Occupants = append([]ResourceOccupant(nil), f.Occupants...)
				for k := range f.Occupants {
					o := &f.Occupants[k]
					o.ID = s.scrub(o.ID, 128)
					o.Name = s.scrub(o.Name, 128)
					o.MemoryUsage = s.scrub(o.MemoryUsage, 128)
				}
			}
		}
		e.Summary = copy
	}
	return e
}

// Require the exact projected values: escapes and masked prefixes must not hide parameters.
func (s *Service) secretArgs(a ToolArgs) bool {
	for _, value := range a.Argv {
		if s.scrub(value, 2048) != value {
			return true
		}
	}
	for _, value := range []string{a.Container, a.Unit, a.Action} {
		if s.scrub(value, 1024) != value {
			return true
		}
	}
	return false
}

func (s *Service) publicCall(c Call) Call {
	c.Args.Argv = append([]string(nil), c.Args.Argv...)
	for i := range c.Args.Argv {
		c.Args.Argv[i] = s.scrub(c.Args.Argv[i], 2048)
	}
	c.Args.Container = s.scrub(c.Args.Container, 1024)
	c.Args.Unit = s.scrub(c.Args.Unit, 1024)
	c.Args.Action = s.scrub(c.Args.Action, 1024)
	c.Object = s.scrub(c.Object, 1024)
	c.ServerName = s.scrub(c.ServerName, 1024)
	c.Output = s.scrub(c.Output, 64<<10)
	c.Error = s.scrub(c.Error, 1024)
	if c.Resources != nil {
		r := *c.Resources
		r.Disks = append([]DiskResource(nil), r.Disks...)
		for i := range r.Disks {
			r.Disks[i].Filesystem = s.scrub(r.Disks[i].Filesystem, 1024)
			r.Disks[i].Mount = s.scrub(r.Disks[i].Mount, 1024)
		}
		c.Resources = &r
	}
	return c
}

func (s *Service) publicConfirmation(c Confirmation) Confirmation {
	c.Calls = append([]BoundCall(nil), c.Calls...)
	for i := range c.Calls {
		c.Calls[i].Object = s.scrub(c.Calls[i].Object, 1024)
	}
	return c
}
