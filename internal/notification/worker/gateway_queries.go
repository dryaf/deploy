package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dryaf/deploy/internal/notification/interaction"
	"github.com/dryaf/deploy/internal/notification/protocol"
	"github.com/dryaf/deploy/internal/safefs"
)

type gatewayQuery struct {
	ID      string              `json:"id"`
	Input   interaction.Inbound `json:"input"`
	Policy  string              `json:"policy"`
	Expires time.Time           `json:"expires"`
	Kind    string              `json:"kind"`
	Worker  string              `json:"worker,omitempty"`
	Command string              `json:"command,omitempty"`
	Root    string              `json:"root,omitempty"`
	Items   []string            `json:"items,omitempty"`
}

func parseGatewayQuery(text string) (kind, worker, id, cursor string, handled bool, err error) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return
	}
	first := fields[0]
	handled = first == "workers" || first == "help" || first == "command" || (len(fields) > 1 && fields[1] == "help")
	if !handled {
		return
	}
	if strings.ContainsAny(text, "\r\n\x00") {
		err = fmt.Errorf("one command per message required")
		return
	}
	if first == "command" {
		if (len(fields) != 2 && len(fields) != 3) || !strings.HasPrefix(fields[1], "C-") || !protocol.ValidToken(fields[1]) {
			err = fmt.Errorf("use command <command-id> [cursor=<cursor>]")
			return
		}
		if len(fields) == 3 {
			var ok bool
			cursor, ok = strings.CutPrefix(fields[2], "cursor=")
			if !ok || !protocol.ValidToken(cursor) {
				err = interaction.Reject("invalid query cursor")
				return
			}
		}
		kind, id = "command", fields[1]
		return
	}
	input := text
	if first == "workers" || first == "help" {
		input = "gateway " + text
	}
	parsed, e := interaction.Parse(input)
	if e != nil {
		err = e
		return
	}
	kind = parsed.Verb
	if first != "workers" && first != "help" {
		worker = parsed.Worker
	}
	for key, value := range parsed.Arguments {
		if key != "cursor" || !protocol.ValidToken(value) {
			err = fmt.Errorf("only a pagination cursor is supported")
			return
		}
		cursor = value
	}
	return
}

func (s *Store) queryDestination(in interaction.Inbound) (d interaction.Destination, ok bool) {
	if s.commandConfig() == nil {
		return
	}
	for _, candidate := range s.commandConfig().Destinations {
		if candidate.ID == in.Conversation && candidate.Provider == in.Provider && candidate.Account == in.Account && candidate.Fingerprint() == in.DestinationFingerprint {
			for _, p := range s.commandConfig().Permissions {
				if p.Actor == in.Actor && p.Conversation == in.Conversation {
					return candidate, true
				}
			}
		}
	}
	return
}

func (s *Store) queryAllowed(q gatewayQuery) bool {
	_, ok := s.queryDestination(q.Input)
	return ok && s.clockSafe() && q.Policy == protocol.Hash(s.commandConfig()) && q.Expires.After(s.now())
}

func readGatewayQuery(tx *sql.Tx, id string) (q gatewayQuery, err error) {
	var body string
	if err = tx.QueryRow("SELECT body FROM gateway_queries WHERE id=?", id).Scan(&body); err != nil {
		return
	}
	err = protocol.Decode([]byte(body), &q, protocol.MaxRequest)
	return
}

func (s *Store) queryItems(tx *sql.Tx, q gatewayQuery) (items []string, err error) {
	c := s.commandConfig()
	available := func(w interaction.Worker, d interaction.Descriptor) bool {
		scope, ok := c.Authorize(q.Input.Actor, q.Input.Conversation, w.Producer.ID, d.Permission)
		return ok && (d.Scoped || scope.WholeWorker)
	}
	switch q.Kind {
	case "workers":
		for _, w := range c.Workers {
			for _, d := range w.Commands {
				if available(w, d) {
					owner, e := s.readOwnership(w.Producer)
					if e != nil {
						return nil, e
					}
					label := w.Alias
					if owner.Retirement != nil {
						label += " (retired)"
					}
					items = append(items, label)
					break
				}
			}
		}
		slices.Sort(items)
	case "help":
		if q.Worker == "" {
			return []string{"workers | <worker> help | <worker> <command> [name=value] | command <id>", "Reply to an enrolled notification, or use an explicit worker and ref=N-….", "Targeted commands require an exact subject and target. Confirmation uses confirm <token>."}, nil
		}
		w, ok := c.Resolve(q.Worker)
		if !ok {
			return nil, interaction.Reject("worker unavailable")
		}
		var names []string
		for name := range w.Commands {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			d := w.Commands[name]
			if available(w, d) {
				items = append(items, w.Alias+" "+name+": "+d.Help)
			}
		}
		if len(items) > 0 {
			owner, e := s.readOwnership(w.Producer)
			if e != nil {
				return nil, e
			}
			if owner.Retirement != nil {
				return []string{w.Alias + " is permanently retired. Use command <id> to inspect historical outcomes."}, nil
			}
		}
	case "command":
		v, e := readCommand(tx, q.Command)
		if e != nil {
			if e == sql.ErrNoRows {
				return nil, interaction.Reject("command unavailable")
			}
			return nil, e
		}
		allowed, ok := c.Authorize(q.Input.Actor, q.Input.Conversation, v.Command.Producer.ID, "read")
		if !ok || !allowed.Allows(v.Command.Scope) || v.Command.Actor != q.Input.Actor || v.Command.Destination.Fingerprint() != q.Input.DestinationFingerprint || !s.commandAuthorized(v.Command, v.Command.Scope) {
			return nil, interaction.Reject("command unavailable")
		}
		if err = expireCommand(tx, &v, s.now()); err != nil {
			return
		}
		items = append(items, fmt.Sprintf("%s: %s; result revision %d; execution conflict %t. Observed %s", v.Command.ID, v.State, v.Revision, v.Conflict, s.now().UTC().Format(time.RFC3339)))
		if v.Revision > 0 {
			var body string
			var result interaction.Result
			if err = tx.QueryRow("SELECT body FROM command_results WHERE command_id=? AND revision=?", v.Command.ID, v.Revision).Scan(&body); err != nil {
				return
			}
			if err = protocol.Decode([]byte(body), &result, protocol.MaxRequest); err != nil {
				return
			}
			if !allowed.Allows(result.Scope) {
				return nil, fmt.Errorf("command result unavailable")
			}
			if result.JobID != "" {
				items = append(items, "Job: "+result.JobID)
			}
			text := result.Text
			for len(text) > 1800 {
				n := 1800
				for !utf8.ValidString(text[:n]) {
					n--
				}
				items = append(items, text[:n])
				text = text[n:]
			}
			if text != "" {
				items = append(items, text)
			}
		}
	default:
		return nil, fmt.Errorf("unknown gateway query")
	}
	if len(items) == 0 {
		items = []string{"No commands available within your current permissions."}
	}
	return
}

// Query results are durable replies, not worker executions or report coverage.
// A page cursor references a frozen listing and cannot widen or renew its scope.
func (s *Store) AdmitGatewayQuery(ctx context.Context, in interaction.Inbound) (handled bool, err error) {
	if s.verificationOnly {
		return false, fmt.Errorf("chat queries unavailable in verification mode")
	}
	if !s.commandIntakeEnabled() {
		return false, fmt.Errorf("chat queries are not admitted")
	}
	if err = s.checkManagedCommandAuthority(ctx); err != nil {
		return false, err
	}
	kind, worker, id, cursor, handled, err := parseGatewayQuery(in.Text)
	if !handled {
		return false, err
	}
	if err != nil {
		return true, interaction.Reject(err.Error())
	}
	d, ok := s.queryDestination(in)
	if !ok || !protocol.ValidToken(in.NativeID) || !protocol.ValidToken(in.Actor) || in.Reply != nil || in.SentAt.IsZero() || len(in.Text) > 4096 || !utf8.ValidString(in.Text) {
		return true, fmt.Errorf("invalid gateway query")
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		if err := guardRequestAdmission(tx, in); err != nil {
			return err
		}
		if !s.clockSafe() {
			return fmt.Errorf("clock uncertain")
		}
		var prior, queryID string
		e := tx.QueryRow("SELECT input_hash,command_id FROM command_inbox WHERE request_key=?", in.RequestKey()).Scan(&prior, &queryID)
		if e == nil {
			if prior != protocol.Hash(in) {
				return fmt.Errorf("inbound identity conflict")
			}
			q, e := readGatewayQuery(tx, queryID)
			if e != nil {
				return e
			}
			if q.Kind == "rejected" {
				return interaction.RejectQuietly("request already rejected")
			}
			if !s.queryAllowed(q) {
				return fmt.Errorf("query no longer authorized")
			}
			return nil
		}
		if e != sql.ErrNoRows {
			return e
		}
		if in.SentAt.After(s.now().Add(5*time.Second)) || s.now().Sub(in.SentAt) > 15*time.Minute {
			return fmt.Errorf("stale query")
		}
		if e = s.reserveAliases(tx); e != nil {
			return e
		}
		if e = s.commandRate(tx, in); e != nil {
			return e
		}
		var pending, own int
		if e = tx.QueryRow("SELECT count(*),COALESCE(sum(json_extract(g.body,'$.input.actor')=? AND json_extract(g.body,'$.input.account')=?),0) FROM command_replies r JOIN gateway_queries g ON g.id=r.command_id WHERE r.state IN ('pending','sending','ambiguous') AND r.kind='gateway'", in.Actor, in.Account).Scan(&pending, &own); e != nil {
			return e
		}
		if pending >= 128 || own >= 16 {
			return fmt.Errorf("gateway reply capacity exhausted")
		}
		q := gatewayQuery{ID: "Q-" + in.RequestKey(), Input: in, Policy: protocol.Hash(s.commandConfig()), Expires: s.now().Add(10 * time.Minute), Kind: kind, Worker: worker, Command: id}
		position := 0
		root := q.ID
		var items []string
		if cursor != "" {
			if e = tx.QueryRow("SELECT root,position FROM gateway_pages WHERE token=?", cursor).Scan(&root, &position); e != nil {
				if e == sql.ErrNoRows {
					return interaction.Reject("page unavailable")
				}
				return e
			}
			old, e := readGatewayQuery(tx, root)
			if e != nil {
				return e
			}
			if !s.queryAllowed(old) || old.Input.Actor != in.Actor || old.Input.Account != in.Account || old.Input.DestinationFingerprint != in.DestinationFingerprint || old.Kind != kind || old.Worker != worker || old.Command != id {
				return interaction.Reject("page scope changed")
			}
			items = old.Items
			q.Root = root
			q.Expires = old.Expires
		} else {
			items, e = s.queryItems(tx, q)
			if e != nil {
				return e
			}
			q.Items = items
		}
		if position < 0 || position >= len(items) {
			return interaction.Reject("invalid page position")
		}
		var page []string
		size := 0
		for position < len(items) && len(page) < 16 {
			item := items[position]
			// queryItems splits oversized details without dropping text.
			if len(item) > 2200 {
				return fmt.Errorf("query item exceeds disclosure page budget")
			}
			if size+len(item) > 2400 && len(page) > 0 {
				break
			}
			page = append(page, item)
			size += len(item) + 1
			position++
		}
		text := strings.Join(page, "\n")
		if position < len(items) {
			var token string
			e = tx.QueryRow("SELECT token FROM gateway_pages WHERE root=? AND position=?", root, position).Scan(&token)
			if e == sql.ErrNoRows {
				token, e = safefs.NewID("page")
				if e != nil {
					return e
				}
				if _, e = tx.Exec("INSERT INTO gateway_pages VALUES(?,?,?)", token, root, position); e != nil {
					return e
				}
			} else if e != nil {
				return e
			}
			prefix := kind
			if worker != "" {
				prefix = worker + " " + kind
			}
			if kind == "command" {
				prefix += " " + id
			}
			text += "\nNext: " + prefix + " cursor=" + token
		}
		body, e := json.Marshal(q)
		if e != nil {
			return e
		}
		if len(body) > protocol.MaxRequest {
			return fmt.Errorf("query snapshot too large")
		}
		if e = s.capacity(tx, "", len(body)+2*protocol.MaxRequest, false); e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO gateway_queries VALUES(?,?)", q.ID, string(body)); e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO command_inbox VALUES(?,?,?)", in.RequestKey(), protocol.Hash(in), q.ID); e != nil {
			return e
		}
		reply, _ := json.Marshal(CommandReply{Kind: "gateway", CommandID: q.ID, Revision: 0, Destination: d, Text: text})
		_, e = tx.Exec("INSERT INTO command_replies(command_id,kind,revision,body) VALUES(?,'gateway',0,?)", q.ID, string(reply))
		return e
	})
	return
}

func (s *Store) gatewayReplyAllowed(tx *sql.Tx, r CommandReply) (bool, error) {
	q, err := readGatewayQuery(tx, r.CommandID)
	if err != nil {
		return false, err
	}
	return s.queryAllowed(q) && r.Destination.Fingerprint() == q.Input.DestinationFingerprint, nil
}
