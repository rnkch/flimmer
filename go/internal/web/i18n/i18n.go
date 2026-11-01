// Package i18n picks the UI language per request and translates strings
// with golang.org/x/text. Source strings (message keys) are English; the
// German texts live in the catalog in messages.go.
package i18n

import (
	"context"
	"net/http"

	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Supported languages; the first one is the fallback.
var Supported = []language.Tag{language.German, language.English}

var matcher = language.NewMatcher(Supported)

// Cookie holds an explicit choice made in the UI ("de" or "en").
const Cookie = "lang"

// FromRequest returns the language from the cookie, else from Accept-Language.
func FromRequest(r *http.Request) language.Tag {
	var prefs []language.Tag
	if c, err := r.Cookie(Cookie); err == nil {
		if t, err := language.Parse(c.Value); err == nil {
			prefs = append(prefs, t)
		}
	}
	if accept, _, err := language.ParseAcceptLanguage(r.Header.Get("Accept-Language")); err == nil {
		prefs = append(prefs, accept...)
	}
	_, i, _ := matcher.Match(prefs...)
	return Supported[i]
}

// Parse accepts a code from the language switch; ok is false for anything
// that isn't supported.
func Parse(code string) (language.Tag, bool) {
	for _, t := range Supported {
		if b, _ := t.Base(); b.String() == code {
			return t, true
		}
	}
	return language.Tag{}, false
}

type ctxKey struct{}

type state struct {
	tag language.Tag
	p   *message.Printer
}

// With stores the language (and its printer) in ctx.
func With(ctx context.Context, tag language.Tag) context.Context {
	return context.WithValue(ctx, ctxKey{}, state{tag, message.NewPrinter(tag, message.Catalog(messages))})
}

func get(ctx context.Context) state {
	if s, ok := ctx.Value(ctxKey{}).(state); ok {
		return s
	}
	return state{Supported[0], message.NewPrinter(Supported[0], message.Catalog(messages))}
}

// T translates key (a fmt format string) for the language in ctx.
func T(ctx context.Context, key string, args ...any) string {
	return get(ctx).p.Sprintf(key, args...)
}

// Lang returns the base language code in ctx, e.g. "de".
func Lang(ctx context.Context) string {
	b, _ := get(ctx).tag.Base()
	return b.String()
}

// Middleware puts the request's language into its context.
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(With(r.Context(), FromRequest(r))))
	})
}
