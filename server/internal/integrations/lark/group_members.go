package lark

import (
	"fmt"
	"strings"
	"time"
)

// GroupMemberPolicy is an explicit operator grant for the named bots only.
// Membership in a group containing that bot grants use of its execution identity,
// never a Multica login or workspace membership.
//
// EDN customization: a single "*" entry opens group-member access for ALL bots
// (current and future) without having to list each App ID.
type GroupMemberPolicy struct {
	AppIDs  map[string]bool
	All     bool
	IdleTTL time.Duration
}

func ParseGroupMemberPolicy(appIDs, idleTTL string) (GroupMemberPolicy, error) {
	p := GroupMemberPolicy{AppIDs: make(map[string]bool), IdleTTL: 2 * time.Hour}
	for _, id := range strings.Split(appIDs, ",") {
		switch id = strings.TrimSpace(id); {
		case id == "*":
			p.All = true
		case id != "":
			p.AppIDs[id] = true
		}
	}
	if v := strings.TrimSpace(idleTTL); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return GroupMemberPolicy{}, fmt.Errorf("MULTICA_LARK_GROUP_SESSION_IDLE_TTL must be a nonnegative duration")
		}
		p.IdleTTL = d
	}
	return p, nil
}

func (p GroupMemberPolicy) Enabled(appID string) bool { return p.All || p.AppIDs[appID] }

// Configured reports whether any bot is enabled, so callers can skip
// decoding the raw event when the policy is off.
func (p GroupMemberPolicy) Configured() bool { return p.All || len(p.AppIDs) > 0 }

func (p GroupMemberPolicy) accepts(inst Installation, msg InboundMessage) bool {
	return p.Enabled(inst.AppID) && msg.AppID == inst.AppID &&
		msg.ChatType == ChatTypeGroup && msg.SenderType == "user" &&
		msg.ChatID != "" && msg.SenderOpenID != "" && msg.MessageID != ""
}
