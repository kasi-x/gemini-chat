package gemini

// Error types mirroring gemini_webapi.exceptions. The CLI prints these as
// "gemini-chat: <message>" on stderr.

type APIError struct{ Msg string }

func (e *APIError) Error() string { return e.Msg }

type GeminiError struct{ Msg string }

func (e *GeminiError) Error() string { return e.Msg }

type UsageLimitExceededError struct{ Msg string }

func (e *UsageLimitExceededError) Error() string { return e.Msg }

type ModelInvalidError struct{ Msg string }

func (e *ModelInvalidError) Error() string { return e.Msg }

type TemporarilyBlockedError struct{ Msg string }

func (e *TemporarilyBlockedError) Error() string { return e.Msg }
