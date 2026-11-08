package capture

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/agentfeedback/agentfeedback/v4/pkg/scrub"
)

func quietString(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}

	return d.String()
}

// Status describes effective capture permission without terminal controls or
// known credential formats from server-provided organisation names.
func Status(p Policy) string {
	if p.State == Enabled && p.Enabled {
		org := p.OrgName
		if strings.TrimSpace(org) == "" {
			org = p.OrgID
		}
		retention := "per the org's retention setting"
		if p.Retention != nil {
			switch p.Retention.Mode {
			case "delete_after_processing":
				retention = "deleted after processing"
			case "retain":
				retention = fmt.Sprintf("%d days", p.Retention.Days)
			}
		}

		return terminalText(fmt.Sprintf("sessions: this org (%s) permits session transcript processing. Policy permits sessions quiet for %s to be scrubbed for known secret formats and uploaded to %s (prompts, responses, tool calls and output). Retention: %s. Sessions under [collect] deny paths stay local; agentfeedback config changes the paths.",
			org, p.Quiet, p.Server, retention))
	}
	var reason string
	switch p.State {
	case Disabled:
		reason = "the org has session processing off"
	case NotApplicable:
		reason = "local mode"
	case Unsupported:
		reason = "this server does not process sessions"
	case Unauthorized:
		reason = "an authorized installation key is required to read the session policy"
	case Suspended:
		reason = "session processing suspended until " + p.RetryAt.UTC().Format(time.RFC3339) +
			"; run agentfeedback sessions status after that time to retry"
	default:
		reason = "session policy unavailable; run agentfeedback doctor to retry"
	}

	return terminalText("sessions: friction reports only (" + reason + ")")
}

func terminalText(s string) string {
	s, counts := scrub.String(s)
	for class := range counts {
		s = strings.ReplaceAll(s, scrub.Marker(class), "[REDACTED]")
	}

	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}

		return r
	}, s)
}
