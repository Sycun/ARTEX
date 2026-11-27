package intercept

import "net/http"
import "net/url"

// AssessmentIdentity is the optional assessment user-agent binding for one task.
// The zero value allows no origin, so requests are unchanged until an identity
// is configured.
type AssessmentIdentity struct {
	userAgent string
	origins   map[string]struct{}
}

func (a AssessmentIdentity) UserAgent() string { return a.userAgent }

func (a AssessmentIdentity) AllowsOrigin(u *url.URL) bool {
	if u == nil || len(a.origins) == 0 {
		return false
	}
	_, ok := a.origins[u.Scheme+"://"+u.Host]
	return ok
}

// ApplyAssessmentUserAgent adds the task's configured identifier to a target
// request. An explicitly supplied User-Agent, including an empty one, takes
// precedence. It reports whether it inserted the identifier.
func ApplyAssessmentUserAgent(req *http.Request) bool {
	scope, ok := TargetScopeFromContext(req.Context())
	if !ok || scope.TaskID == "" || scope.ConversationID != 0 || !scope.AssessmentIdentity.AllowsOrigin(req.URL) {
		return false
	}
	if _, explicit := req.Header["User-Agent"]; explicit {
		return false
	}
	req.Header.Set("User-Agent", scope.AssessmentIdentity.UserAgent())
	return true
}
