package management

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	oauthSessionTTL          = 30 * time.Minute
	oauthCompletedSessionTTL = time.Minute
	maxOAuthStateLength      = 128
	maxOAuthNonceLength      = 128
)

var (
	errInvalidOAuthState      = errors.New("invalid oauth state")
	errUnsupportedOAuthFlow   = errors.New("unsupported oauth provider")
	errOAuthSessionNotPending = errors.New("oauth session is not pending")
)

type oauthSession struct {
	Provider  string
	AuthDir   string
	Status    string
	NonceHash string
	Saving    bool
	Completed bool
	CreatedAt time.Time
	ExpiresAt time.Time
	cancel    context.CancelFunc
}

type oauthSessionStore struct {
	mu           sync.RWMutex
	ttl          time.Duration
	completedTTL time.Duration
	sessions     map[string]oauthSession
}

func newOAuthSessionStore(ttl time.Duration) *oauthSessionStore {
	if ttl <= 0 {
		ttl = oauthSessionTTL
	}
	completedTTL := oauthCompletedSessionTTL
	if ttl < completedTTL {
		completedTTL = ttl
	}
	return &oauthSessionStore{
		ttl:          ttl,
		completedTTL: completedTTL,
		sessions:     make(map[string]oauthSession),
	}
}

func (s *oauthSessionStore) purgeExpiredLocked(now time.Time) {
	for state, session := range s.sessions {
		if !session.ExpiresAt.IsZero() && !now.Before(session.ExpiresAt) {
			if session.cancel != nil {
				session.cancel()
			}
			delete(s.sessions, state)
		}
	}
}

func (s *oauthSessionStore) register(state, provider, nonce string, authDir ...string) {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	capturedAuthDir := ""
	if len(authDir) > 0 {
		capturedAuthDir = strings.TrimSpace(authDir[0])
	}
	if state == "" || provider == "" {
		return
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	if previous, ok := s.sessions[state]; ok && previous.cancel != nil {
		previous.cancel()
	}
	s.sessions[state] = oauthSession{
		Provider:  provider,
		AuthDir:   capturedAuthDir,
		NonceHash: hashOAuthNonce(nonce),
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
}

func (s *oauthSessionStore) setErrorIfPending(state, provider, message string) bool {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	message = strings.TrimSpace(message)
	if state == "" {
		return false
	}
	if message == "" {
		message = "Authentication failed"
	}
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.Completed || session.Saving || session.Status != "" ||
		(provider != "" && !strings.EqualFold(session.Provider, provider)) {
		s.mu.Unlock()
		return false
	}
	cancel := session.cancel
	session.cancel = nil
	session.Status = message
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return true
}

func (s *oauthSessionStore) setError(state, message string) {
	state = strings.TrimSpace(state)
	message = strings.TrimSpace(message)
	if state == "" {
		return
	}
	if message == "" {
		message = "Authentication failed"
	}
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.Completed {
		s.mu.Unlock()
		return
	}
	cancel := session.cancel
	session.cancel = nil
	session.Status = message
	session.Saving = false
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (s *oauthSessionStore) complete(state string) {
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.Completed {
		s.mu.Unlock()
		return
	}
	cancel := session.cancel
	session.cancel = nil
	session.Status = ""
	session.Saving = false
	session.Completed = true
	session.ExpiresAt = now.Add(s.completedTTL)
	s.sessions[state] = session
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (s *oauthSessionStore) get(state string) (oauthSession, bool) {
	state = strings.TrimSpace(state)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	session.cancel = nil
	return session, ok
}

func (s *oauthSessionStore) isPending(state, provider string) bool {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.Completed || session.Saving || session.Status != "" {
		return false
	}
	if provider == "" {
		return true
	}
	return strings.EqualFold(session.Provider, provider)
}

func (s *oauthSessionStore) attachCancel(state, provider string, cancel context.CancelFunc) bool {
	if cancel == nil {
		return false
	}
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.Completed || session.Saving || session.Status != "" ||
		(provider != "" && !strings.EqualFold(session.Provider, provider)) {
		s.mu.Unlock()
		cancel()
		return false
	}
	previous := session.cancel
	session.cancel = cancel
	s.sessions[state] = session
	s.mu.Unlock()

	if previous != nil {
		previous()
	}
	return true
}

func (s *oauthSessionStore) claimForSave(state, provider string) bool {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.Completed || session.Saving || session.Status != "" ||
		(provider != "" && !strings.EqualFold(session.Provider, provider)) {
		s.mu.Unlock()
		return false
	}
	cancel := session.cancel
	session.cancel = nil
	session.Saving = true
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	return true
}

func (s *oauthSessionStore) cancel(state string) bool {
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.Completed || session.Saving || session.Status != "" {
		s.mu.Unlock()
		return false
	}
	delete(s.sessions, state)
	s.mu.Unlock()

	if session.cancel != nil {
		session.cancel()
	}
	return true
}

var oauthSessions = newOAuthSessionStore(oauthSessionTTL)

func registerOAuthSession(state, provider, nonce string) {
	oauthSessions.register(state, provider, nonce)
}

func registerOAuthSessionForAuthDir(state, provider, nonce, authDir string) {
	oauthSessions.register(state, provider, nonce, authDir)
}

func setOAuthSessionError(state, message string) { oauthSessions.setError(state, message) }

func setPendingOAuthSessionError(state, provider, message string) bool {
	return oauthSessions.setErrorIfPending(state, provider, message)
}

func completeOAuthSession(state string) { oauthSessions.complete(state) }

func getOAuthSession(state string) (provider string, status string, ok bool) {
	session, ok := oauthSessions.get(state)
	if !ok || session.Completed || session.Saving {
		return "", "", false
	}
	return session.Provider, session.Status, true
}

func getOAuthSessionForCallback(state string) (provider, status, authDir string, ok bool) {
	session, ok := oauthSessions.get(state)
	if !ok || session.Completed || session.Saving {
		return "", "", "", false
	}
	return session.Provider, session.Status, session.AuthDir, true
}

func getOAuthSessionDetails(state string) (provider string, status string, completed bool, ok bool) {
	session, ok := oauthSessions.get(state)
	if !ok {
		return "", "", false, false
	}
	return session.Provider, session.Status, session.Completed, true
}

func verifyOAuthNonce(state, nonce string) bool {
	session, ok := oauthSessions.get(state)
	if !ok {
		return false
	}
	if session.NonceHash == "" {
		return true
	}
	supplied := hashOAuthNonce(nonce)
	return supplied != "" && subtle.ConstantTimeCompare([]byte(session.NonceHash), []byte(supplied)) == 1
}

func isOAuthSessionPending(state, provider string) bool {
	return oauthSessions.isPending(state, provider)
}

func setOAuthSessionCancel(state, provider string, cancel context.CancelFunc) bool {
	return oauthSessions.attachCancel(state, provider, cancel)
}

func IsOAuthSessionNotPending(err error) bool {
	return errors.Is(err, errOAuthSessionNotPending)
}

func guardOAuthSessionPendingForSave(state, provider string) error {
	if oauthSessions.claimForSave(state, provider) {
		return nil
	}
	return errOAuthSessionNotPending
}

func cancelOAuthSession(state string) bool {
	return oauthSessions.cancel(state)
}

func hashOAuthNonce(nonce string) string {
	trimmed := strings.TrimSpace(nonce)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) > maxOAuthNonceLength {
		trimmed = trimmed[:maxOAuthNonceLength]
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

func validateOAuthState(state string) error {
	trimmed := strings.TrimSpace(state)
	if trimmed == "" {
		return fmt.Errorf("%w: empty", errInvalidOAuthState)
	}
	if len(trimmed) > maxOAuthStateLength {
		return fmt.Errorf("%w: too long", errInvalidOAuthState)
	}
	if strings.Contains(trimmed, "/") || strings.Contains(trimmed, "\\") {
		return fmt.Errorf("%w: contains path separator", errInvalidOAuthState)
	}
	if strings.Contains(trimmed, "..") {
		return fmt.Errorf("%w: contains '..'", errInvalidOAuthState)
	}
	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '.':
		default:
			return fmt.Errorf("%w: invalid character", errInvalidOAuthState)
		}
	}
	return nil
}

func normalizeOAuthProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "anthropic", "claude":
		return "anthropic", nil
	case "codex", "openai":
		return "codex", nil
	case "gemini", "google":
		return "gemini", nil
	case "iflow", "i-flow":
		return "iflow", nil
	case "antigravity", "anti-gravity":
		return "antigravity", nil
	case "qwen":
		return "qwen", nil
	case "kimi":
		return "kimi", nil
	case "xai", "x-ai", "x.ai", "grok":
		return "xai", nil
	default:
		return "", errUnsupportedOAuthFlow
	}
}

func oauthSessionErrorWithCause(message string, cause error) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "Authentication failed"
	}
	if cause == nil {
		return message
	}
	detail := strings.TrimSpace(cause.Error())
	if detail == "" {
		return message
	}
	return message + ": " + detail
}

type oauthCallbackFilePayload struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error"`
}

func writeOAuthCallbackFile(authDir, provider, state, code, errorMessage string) (string, error) {
	if strings.TrimSpace(authDir) == "" {
		return "", fmt.Errorf("auth dir is empty")
	}
	canonicalProvider, err := normalizeOAuthProvider(provider)
	if err != nil {
		return "", err
	}
	if err := validateOAuthState(state); err != nil {
		return "", err
	}

	fileName := fmt.Sprintf(".oauth-%s-%s.oauth", canonicalProvider, state)
	filePath := filepath.Join(authDir, fileName)
	payload := oauthCallbackFilePayload{
		Code:  strings.TrimSpace(code),
		State: strings.TrimSpace(state),
		Error: strings.TrimSpace(errorMessage),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal oauth callback payload: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0o600); err != nil {
		return "", fmt.Errorf("write oauth callback file: %w", err)
	}
	return filePath, nil
}

func WriteOAuthCallbackFileForPendingSession(
	provider string,
	state string,
	code string,
	errorMessage string,
) (string, error) {
	canonicalProvider, err := normalizeOAuthProvider(provider)
	if err != nil {
		return "", err
	}
	sessionProvider, status, authDir, ok := getOAuthSessionForCallback(state)
	if !ok || status != "" || !strings.EqualFold(sessionProvider, canonicalProvider) {
		return "", errOAuthSessionNotPending
	}
	if strings.TrimSpace(authDir) == "" {
		return "", fmt.Errorf("oauth session auth dir is empty")
	}
	return writeOAuthCallbackFile(authDir, canonicalProvider, state, code, errorMessage)
}
