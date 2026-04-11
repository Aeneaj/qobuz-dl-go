package api

import "fmt"

type AuthenticationError struct{ msg string }
type IneligibleError struct{ msg string }
type InvalidAppIDError struct{ msg string }
type InvalidAppSecretError struct{ msg string }
type InvalidQualityError struct{ msg string }
type NonStreamableError struct{ msg string }

func (e *AuthenticationError) Error() string  { return fmt.Sprintf("authentication error: %s", e.msg) }
func (e *IneligibleError) Error() string      { return fmt.Sprintf("ineligible: %s", e.msg) }
func (e *InvalidAppIDError) Error() string    { return fmt.Sprintf("invalid app id: %s", e.msg) }
func (e *InvalidAppSecretError) Error() string { return fmt.Sprintf("invalid app secret: %s", e.msg) }
func (e *InvalidQualityError) Error() string  { return fmt.Sprintf("invalid quality: %s", e.msg) }
func (e *NonStreamableError) Error() string   { return fmt.Sprintf("non streamable: %s", e.msg) }
