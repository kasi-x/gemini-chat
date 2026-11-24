// gemini-chat sends a prompt to browser Gemini (gemini.google.com) through
// the internal StreamGenerate API — no browser needed. Go port of
// /Users/user/dotfiles/tools/bin/gemini-chat.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kasi-x/gemini-chat/internal/auth"
	"github.com/kasi-x/gemini-chat/internal/gemini"
	"github.com/kasi-x/gemini-chat/internal/state"
)

type options struct {
	account          string
	newChat          bool
	model            string
	deepResearch     bool
	extendedThinking bool
	temporary        bool
	timeout          float64
	researchTimeout  float64
	list             bool
	listModels       bool
	jsonOut          bool
	prompt           string
}

func parseArgs(argv []string) (options, error) {
	var o options
	fs := flag.NewFlagSet("gemini-chat", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.account, "account", "", "gx account name (default: round-robin)")
	fs.StringVar(&o.account, "a", "", "gx account name (default: round-robin)")
	fs.BoolVar(&o.newChat, "new", false, "start a new chat (default: continue)")
	fs.StringVar(&o.model, "model", "", "model name from list_models()")
	fs.StringVar(&o.model, "m", "", "model name from list_models()")
	fs.BoolVar(&o.deepResearch, "deep-research", false, "run Deep Research")
	fs.BoolVar(&o.extendedThinking, "extended-thinking", false, "extended thinking")
	fs.BoolVar(&o.temporary, "temporary", false, "temporary chat (not saved)")
	fs.Float64Var(&o.timeout, "timeout", 300, "HTTP timeout seconds")
	fs.Float64Var(&o.researchTimeout, "research-timeout", 900, "deep research wait seconds")
	fs.BoolVar(&o.list, "list", false, "list accounts and rotation state")
	fs.BoolVar(&o.listModels, "list-models", false, "list available models")
	fs.BoolVar(&o.jsonOut, "json", false, "JSON output")

	if err := fs.Parse(argv); err != nil {
		return o, err
	}
	if !o.list && !o.listModels {
		args := fs.Args()
		if len(args) > 0 {
			o.prompt = args[0]
		} else {
			if termIsTTY() {
				return o, fmt.Errorf("prompt required (arg or stdin)")
			}
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return o, err
			}
			o.prompt = string(data)
		}
		o.prompt = strings.TrimSpace(o.prompt)
		if o.prompt == "" {
			return o, fmt.Errorf("prompt must not be empty")
		}
	}
	return o, nil
}

func termIsTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "gemini-chat: %v\n", err)
	os.Exit(1)
}

// isStaleConversation mirrors _is_stale_conversation.
func isStaleConversation(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(err.Error(), "1097") ||
		strings.Contains(err.Error(), "Failed to generate contents") ||
		strings.Contains(text, "not found") ||
		strings.Contains(text, "permission")
}

func main() {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		fatal(err)
	}
	if err := run(opts); err != nil {
		fatal(err)
	}
}

func run(o options) error {
	accounts, err := auth.ResolveAccounts()
	if err != nil {
		return err
	}
	st := state.Load()
	acct, err := auth.PickAccount(accounts, o.account, st.LastAuthUser)
	if err != nil {
		return err
	}

	if o.list {
		fmt.Printf("state: %s\n", state.Path())
		last := st.LastAuthUser
		if last == "" {
			last = "(none)"
		}
		fmt.Printf("last_authuser: %s\n", last)
		for _, a := range accounts {
			saved := st.Chats[a.Name] != nil
			line := fmt.Sprintf("  %-10s %-32s authuser=%d", a.Name, a.Email, a.AuthUser)
			if saved {
				line += "  chat=yes"
			}
			fmt.Println(line)
		}
		return nil
	}

	httpClient, _, _, err := auth.LoadCookieJar()
	if err != nil {
		return err
	}

	client := &gemini.Client{HTTP: httpClient, AuthUser: acct.AuthUser}
	timeout := time.Duration(o.timeout * float64(time.Second))
	if err := client.Init(context.Background(), timeout); err != nil {
		return fmt.Errorf("Gemini 初期化に失敗 (%s)\n  cookie が失効している可能性があります。Chromium :9334 で再ログイン後\n  'task lightpanda:sync' を実行してください", err.Error())
	}

	if o.listModels {
		for _, m := range client.ListModels() {
			fmt.Printf("%s\t%s\n", m.ModelName, m.DisplayName)
		}
		return nil
	}

	var model *gemini.AvailableModel
	var modelName any
	if o.model != "" {
		m, err := client.ResolveModel(o.model)
		if err != nil {
			names := make([]string, 0)
			for _, mm := range client.ListModels() {
				names = append(names, mm.ModelName)
			}
			avail := strings.Join(names, ", ")
			if avail == "" {
				avail = "(none)"
			}
			return fmt.Errorf("unknown model '%s'; available: %s", o.model, avail)
		}
		model = m
		modelName = m.ModelName
	}

	var chat *gemini.ChatSession
	if !o.newChat {
		if saved := st.Chats[acct.Name]; saved != nil && len(saved.Metadata) > 0 {
			chat = gemini.ResumeChatSession(saved.Metadata, saved.CID, saved.RID, saved.RCID)
		}
	}
	if chat == nil {
		chat = gemini.NewChatSession()
	}

	staleNote := ""
	text, thoughts, err := client.SendMessage(context.Background(), chat, o.prompt, model,
		o.extendedThinking, o.temporary, timeout)
	if err != nil && !o.newChat && isStaleConversation(err) {
		staleNote = "前の会話が失効していたため新規チャットで再送しました"
		chat = gemini.NewChatSession()
		text, thoughts, err = client.SendMessage(context.Background(), chat, o.prompt, model,
			o.extendedThinking, o.temporary, timeout)
	}
	if err != nil {
		if o.deepResearch && strings.Contains(strings.ToLower(err.Error()), "deep research plan") {
			return fmt.Errorf("Deep Research のプランが返りませんでした。\n  gemini-webapi が現在の Web API に未対応の可能性があります。\n  gemini-browser スキル (DOM 手順, Chromium :9334) で実行してください")
		}
		return err
	}
	if o.deepResearch {
		return fmt.Errorf("Deep Research のプランが返りませんでした。\n  gemini-webapi が現在の Web API に未対応の可能性があります。\n  gemini-browser スキル (DOM 手順, Chromium :9334) で実行してください")
	}

	cid, rid, rcid := "", "", ""
	if len(chat.Metadata) > 0 {
		if v, ok := chat.Metadata[0].(string); ok {
			cid = v
		}
		if len(chat.Metadata) > 1 {
			if v, ok := chat.Metadata[1].(string); ok {
				rid = v
			}
		}
		if len(chat.Metadata) > 2 {
			if v, ok := chat.Metadata[2].(string); ok {
				rcid = v
			}
		}
	}
	if err := st.SaveChat(acct.Name, acct.AuthUser, chat.Metadata, cid, rid, rcid); err != nil {
		return err
	}

	if o.jsonOut {
		out := struct {
			Account      string `json:"account"`
			Email        string `json:"email"`
			AuthUser     int    `json:"authuser"`
			Model        any    `json:"model"`
			DeepResearch bool   `json:"deep_research"`
			NewChat      bool   `json:"new_chat"`
			Note         string `json:"note"`
			Text         string `json:"text"`
			Thoughts     string `json:"thoughts"`
		}{
			Account:      acct.Name,
			Email:        acct.Email,
			AuthUser:     acct.AuthUser,
			Model:        modelName,
			DeepResearch: o.deepResearch,
			NewChat:      o.newChat || staleNote != "",
			Note:         staleNote,
			Text:         text,
			Thoughts:     thoughts,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}

	if staleNote != "" {
		fmt.Fprintf(os.Stderr, "[note] %s\n", staleNote)
	}
	if thoughts != "" {
		fmt.Fprintf(os.Stderr, "[thinking]\n%s\n\n", thoughts)
	}
	fmt.Println(text)
	return nil
}
