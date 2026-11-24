// Package gemini implements the Gemini web (gemini.google.com) internal API
// client used by gemini-chat: cookie-jar based init (SNlM0e token extraction),
// the StreamGenerate endpoint, and the batchexecute RPC used for model
// discovery. It is a Go port of the relevant parts of the Python
// gemini_webapi library, restricted to what the CLI needs.
package gemini
