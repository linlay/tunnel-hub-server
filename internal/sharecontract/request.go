package sharecontract

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

var shareIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
var resourceIDPattern = regexp.MustCompile(`^[a-f0-9]{24}$`)

// Selection is the only authority a public query may supply. Receiver, version,
// context and binding always come from the stored envelope.
type Selection struct {
	Action     Action
	ResourceID string
}

func ValidShareID(value string) bool { return shareIDPattern.MatchString(value) }

// ParseQuery is for the new public entry. The old attachment locator is handled
// only by the conversation adapter. Unknown display parameters are inert.
func ParseQuery(raw string) (Selection, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return Selection{}, errors.New("invalid share query")
	}
	for key, entries := range values {
		switch key {
		case "action", "resourceId":
			if len(entries) != 1 || entries[0] == "" {
				return Selection{}, errors.New("invalid share parameter")
			}
		case "receiverId", "contextVersion", "context", "resourceVersion", "owner", "userId", "token", "url", "attachment", "webAppId", "deviceId", "binding", "upstream", "targetUrl", "publicHost":
			return Selection{}, errors.New("share descriptor cannot be overridden")
		}
	}
	action := Action(values.Get("action"))
	if action == "" {
		action = Preview
	}
	switch action {
	case Preview, Desktop, Download, Service, Resolve:
	default:
		return Selection{}, errors.New("unsupported share action")
	}
	id := values.Get("resourceId")
	if (action == Download && !resourceIDPattern.MatchString(id)) || (action != Download && id != "") {
		return Selection{}, errors.New("invalid share resource selection")
	}
	return Selection{Action: action, ResourceID: id}, nil
}

// ValidResourcePath accepts canonical ASCII paths only: no URL decoding, dot
// segments, escapes or OS-specific normalization is needed at serving time.
func ValidResourcePath(value string) bool {
	if value == "" || len(value) > 255 || strings.HasPrefix(value, "/") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, char := range segment {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
				char >= '0' && char <= '9' || strings.ContainsRune("._-", char)) {
				return false
			}
		}
	}
	return true
}
