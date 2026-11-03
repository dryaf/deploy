// Package interaction defines provider-independent command contracts. Domain
// workers own execution and must enforce scope before querying their own data.
package interaction

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/dryaf/deploy/internal/notification/protocol"
)

const Version = 1

type Scope struct {
	WholeWorker bool     `json:"whole_worker,omitempty"`
	Subjects    []string `json:"subjects,omitempty"`
}

func (s Scope) Validate() error {
	if s.WholeWorker {
		if len(s.Subjects) != 0 {
			return fmt.Errorf("ambiguous scope")
		}
		return nil
	}
	if len(s.Subjects) == 0 || len(s.Subjects) > 128 {
		return fmt.Errorf("invalid scope")
	}
	prev := ""
	for _, id := range s.Subjects {
		if !protocol.ValidToken(id) || id <= prev {
			return fmt.Errorf("scope must be sorted and unique")
		}
		prev = id
	}
	return nil
}
func (s Scope) Contains(id string) bool { return s.WholeWorker || slices.Contains(s.Subjects, id) }
func (s Scope) Allows(other Scope) bool {
	if other.Validate() != nil {
		return false
	}
	if s.WholeWorker {
		return true
	}
	if other.WholeWorker {
		return false
	}
	for _, id := range other.Subjects {
		if !s.Contains(id) {
			return false
		}
	}
	return true
}

type Argument struct {
	Kind     string `json:"kind"` // token, text, duration
	Required bool   `json:"required,omitempty"`
	Max      int    `json:"max"` // runes or duration seconds
}
type Descriptor struct {
	ID             string              `json:"id"`
	Version        int                 `json:"version"`
	Permission     string              `json:"permission"`
	Effect         string              `json:"effect"` // read or mutation
	Confirm        bool                `json:"confirm,omitempty"`
	Scoped         bool                `json:"scoped,omitempty"`
	TargetRequired bool                `json:"target_required,omitempty"`
	Arguments      map[string]Argument `json:"arguments,omitempty"`
	Help           string              `json:"help"`
}
type Worker struct {
	Alias      string                `json:"alias"`
	Producer   protocol.Producer     `json:"producer"`
	Generation string                `json:"generation"`
	Commands   map[string]Descriptor `json:"commands"`
}
type Destination struct {
	ID         string `json:"id"`
	Generation string `json:"generation"`
	Provider   string `json:"provider"`
	Account    string `json:"account"`
	NativeID   string `json:"native_id"`
	Thread     string `json:"thread,omitempty"`
}

func (d Destination) Fingerprint() string { return protocol.Hash(d) }

type Permission struct {
	Actor        string   `json:"actor"`
	Conversation string   `json:"conversation"`
	Worker       string   `json:"worker"`
	Permissions  []string `json:"permissions"`
	Scope        Scope    `json:"scope"`
}
type Config struct {
	Revision     string        `json:"revision"`
	Workers      []Worker      `json:"workers"`
	Destinations []Destination `json:"destinations"`
	Permissions  []Permission  `json:"permissions"`
}

func (c Config) Validate() error {
	if !protocol.ValidToken(c.Revision) || len(c.Workers) == 0 || len(c.Workers) > 128 || len(c.Destinations) == 0 || len(c.Destinations) > 64 || len(c.Permissions) > 1024 {
		return fmt.Errorf("invalid interaction enrollment")
	}
	aliases := map[string]bool{}
	ids := map[string]bool{}
	for _, w := range c.Workers {
		if !protocol.ValidToken(w.Alias) || aliases[w.Alias] || !w.Producer.Valid() || ids[w.Producer.ID] || !protocol.ValidToken(w.Generation) || len(w.Commands) == 0 || len(w.Commands) > 32 {
			return fmt.Errorf("invalid worker manifest")
		}
		aliases[w.Alias] = true
		ids[w.Producer.ID] = true
		descriptorIDs := map[string]bool{}
		for name, d := range w.Commands {
			if descriptorIDs[d.ID] {
				return fmt.Errorf("duplicate command contract identity")
			}
			descriptorIDs[d.ID] = true
			if !protocol.ValidToken(name) || strings.ContainsAny(name, "/:") || slices.Contains([]string{"workers", "help", "command", "confirm"}, name) || !protocol.ValidToken(d.ID) || d.Version != Version || !slices.Contains([]string{"read", "incident.manage", "job.request", "risk.accept"}, d.Permission) || !slices.Contains([]string{"read", "mutation"}, d.Effect) || len(d.Arguments) > 16 || len(d.Help) > 1024 {
				return fmt.Errorf("invalid command descriptor")
			}
			if d.Effect == "read" && (d.Permission != "read" || d.Confirm) {
				return fmt.Errorf("invalid read descriptor")
			}
			if d.Effect == "mutation" && d.Permission == "read" {
				return fmt.Errorf("mutation requires write permission")
			}
			if name == "accept-risk" && (!d.Confirm || d.Permission != "risk.accept" || d.Effect != "mutation" || !d.TargetRequired) {
				return fmt.Errorf("risk acceptance requires scoped confirmation")
			}
			// Revocation narrows an existing decision. It still needs the risk
			// permission and an exact subject/target, but no extra confirmation.
			if (d.Permission == "risk.accept" && ((!d.Confirm && name != "revoke-acceptance") || d.Effect != "mutation" || !d.TargetRequired)) || (d.TargetRequired && !d.Scoped) {
				return fmt.Errorf("targeted mutation requires scoped confirmation policy")
			}
			if d.TargetRequired {
				for _, key := range []string{"subject", "target"} {
					a, ok := d.Arguments[key]
					if !ok || !a.Required || a.Kind != "token" {
						return fmt.Errorf("targeted commands require explicit subject and target identifiers")
					}
				}
			}
			for key, a := range d.Arguments {
				if key == "ref" {
					return fmt.Errorf("ref is reserved for notification routing")
				}
				if !protocol.ValidToken(key) || strings.ContainsAny(key, "/:") || !slices.Contains([]string{"text", "token", "duration"}, a.Kind) || a.Max < 1 || a.Max > 90*86400 {
					return fmt.Errorf("invalid argument descriptor")
				}
			}
		}
	}
	conversations := map[string]bool{}
	native := map[string]bool{}
	accounts := map[string]string{}
	counts := map[string]int{}
	for _, d := range c.Destinations {
		if !protocol.ValidToken(d.ID) || conversations[d.ID] || !protocol.ValidToken(d.Generation) || !slices.Contains([]string{"signal", "telegram"}, d.Provider) || !protocol.ValidToken(d.Account) || d.NativeID == "" || len(d.NativeID) > 256 || strings.ContainsAny(d.NativeID, "\x00\r\n") || len(d.Thread) > 256 {
			return fmt.Errorf("invalid conversation")
		}
		if old := accounts[d.Provider]; old != "" && old != d.Account {
			return fmt.Errorf("only one account per provider")
		}
		accounts[d.Provider] = d.Account
		counts[d.Provider]++
		if counts[d.Provider] > 32 {
			return fmt.Errorf("too many conversations")
		}
		key := protocol.Hash([]string{d.Provider, d.Account, d.NativeID, d.Thread})
		if native[key] {
			return fmt.Errorf("duplicate destination")
		}
		native[key] = true
		conversations[d.ID] = true
	}
	grants := map[string]bool{}
	for _, p := range c.Permissions {
		key := protocol.Hash([]string{p.Actor, p.Conversation, p.Worker})
		if !protocol.ValidToken(p.Actor) || !conversations[p.Conversation] || !ids[p.Worker] || p.Scope.Validate() != nil || grants[key] || len(p.Permissions) == 0 {
			return fmt.Errorf("invalid permission")
		}
		grants[key] = true
		for _, v := range p.Permissions {
			if !slices.Contains([]string{"read", "incident.manage", "job.request", "risk.accept"}, v) {
				return fmt.Errorf("unknown permission")
			}
		}
	}
	return nil
}
func (c Config) Resolve(alias string) (Worker, bool) {
	for _, w := range c.Workers {
		if w.Alias == alias {
			return w, true
		}
	}
	return Worker{}, false
}
func (c Config) Authorize(actor, conversation, worker, permission string) (Scope, bool) {
	for _, p := range c.Permissions {
		if p.Actor == actor && p.Conversation == conversation && p.Worker == worker && slices.Contains(p.Permissions, permission) {
			return p.Scope, true
		}
	}
	return Scope{}, false
}

type Inbound struct {
	Provider               string       `json:"provider"`
	Account                string       `json:"account"`
	NativeID               string       `json:"native_id"`
	Actor                  string       `json:"actor"`
	Conversation           string       `json:"conversation"`
	DestinationFingerprint string       `json:"destination_fingerprint"`
	SentAt                 time.Time    `json:"sent_at"`
	Text                   string       `json:"text"`
	Reply                  *NativeReply `json:"reply,omitempty"`
}

// NativeReply contains provider identity metadata; quoted text is never authority.
type NativeReply struct {
	ID     string `json:"id"`
	Sender string `json:"sender"`
}
type DeliveryBinding struct {
	Destination Destination `json:"destination"`
	Native      NativeReply `json:"native"`
}
type BoundContext struct {
	Reference string                      `json:"reference"`
	Context   protocol.InteractionContext `json:"context"`
}

func (i Inbound) RequestKey() string {
	return protocol.Hash([]string{"deploy-command-request-v1", i.Provider, i.Account, i.NativeID})
}
func (i Inbound) CommandID() string { return "C-" + i.RequestKey() }

type Command struct {
	Context        *BoundContext     `json:"context,omitempty"`
	ID             string            `json:"id"`
	RequestKey     string            `json:"request_key"`
	InputHash      string            `json:"input_hash"`
	Producer       protocol.Producer `json:"producer"`
	Generation     string            `json:"generation"`
	Actor          string            `json:"actor"`
	Destination    Destination       `json:"destination"`
	PolicyRevision string            `json:"policy_revision"`
	Descriptor     Descriptor        `json:"descriptor"`
	Arguments      map[string]string `json:"arguments"`
	Scope          Scope             `json:"scope"`
	IssuedAt       time.Time         `json:"issued_at"`
	Deadline       time.Time         `json:"deadline"`
}
type View struct {
	Command      Command            `json:"command"`
	State        string             `json:"state"`
	Conflict     bool               `json:"execution_conflict,omitempty"`
	Revision     uint64             `json:"result_revision"`
	Prepared     *Preparation       `json:"preparation,omitempty"`
	Grant        *Grant             `json:"grant,omitempty"`
	NonAdmission *NonAdmissionProof `json:"non_admission,omitempty"`
}
type NonAdmissionProof struct {
	Identity    protocol.Identity `json:"identity"`
	Producer    protocol.Producer `json:"producer"`
	CommandID   string            `json:"command_id"`
	CommandHash string            `json:"command_hash"`
	GrantID     string            `json:"grant_id"`
	ID          string            `json:"id"`
}
type Preparation struct {
	Hash      string    `json:"hash"`
	Summary   string    `json:"summary"`
	ExpiresAt time.Time `json:"expires_at"`
}
type Grant struct {
	Identity        protocol.Identity `json:"identity"`
	ID              string            `json:"id"`
	CommandID       string            `json:"command_id"`
	CommandHash     string            `json:"command_hash"`
	PreparationHash string            `json:"preparation_hash,omitempty"`
	ExpiresAt       time.Time         `json:"expires_at"`
}
type Result struct {
	CommandID   string       `json:"command_id"`
	CommandHash string       `json:"command_hash"`
	Revision    uint64       `json:"revision"`
	Phase       string       `json:"phase"`
	State       string       `json:"state"`
	Scope       Scope        `json:"scope"`
	Text        string       `json:"text"`
	JobID       string       `json:"job_id,omitempty"`
	Preparation *Preparation `json:"preparation,omitempty"`
}
type ResultReceipt struct {
	Identity           protocol.Identity `json:"identity"`
	Producer           protocol.Producer `json:"producer"`
	CommandID          string            `json:"command_id"`
	Revision           uint64            `json:"revision"`
	Hash               string            `json:"hash"`
	Disposition        string            `json:"disposition"`
	TerminalState      string            `json:"terminal_state,omitempty"`
	TerminalDecisionID string            `json:"terminal_decision_id,omitempty"`
	NonAdmission       bool              `json:"non_admission,omitempty"`
}

func Terminal(s string) bool {
	return slices.Contains([]string{"succeeded", "failed", "expired", "rejected"}, s)
}
