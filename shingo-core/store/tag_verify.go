package store

// TagVerifyResult holds the result of a tag verification check.
//
// Phase 6.4a moved the VerifyTag method body to
// shingocore/service/tag_verify_service.go::TagVerifyService.VerifyTag.
// The TagVerifyResult struct stays in package store as a public type
// because the type is the wire shape returned to messaging callers
// and lives in the persistence layer's vocabulary.
type TagVerifyResult struct {
	Match    bool
	Expected string
	Detail   string
}
