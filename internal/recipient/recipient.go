package recipient

type Type string

const (
	User  Type = "user"
	Group Type = "group"
)

type Target struct {
	Type   Type   `json:"type"`
	OpenID string `json:"openid"`
}
