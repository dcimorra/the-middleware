package chat

import "strings"

// OperatorRules is how the operator's own rules (settings: agents.rules)
// are said to an agent, or "" when there are none. They close every
// preamble the house writes: a site's conversation, the house's, the
// network agent, a crew agent, and a site's own conversation that can
// change files, with whatever provider and model runs the turn.
//
// They are the operator's and not the guide's. A rule about icons and
// finished interfaces was first written into the embedded guide and the
// preamble, where it reached every installation of a public project as
// if one person's taste were how the house works. What an operator wants
// of every agent is a setting; the guide says only what is true of the
// house.
func OperatorRules(rules string) string {
	rules = strings.TrimSpace(rules)
	if rules == "" {
		return ""
	}
	return "\nThe operator of this installation wrote the rules below for every agent that works here, whichever model it is. " +
		"They hold in every turn without being repeated, and a request that does not mention them does not lift them; only the person you are working for, saying otherwise in this conversation, does. " +
		"They never widen what you may do: where one asks for something the rest of this text forbids, the rest of this text holds.\n" +
		"<operator-rules>\n" + rules + "\n</operator-rules>\n"
}
