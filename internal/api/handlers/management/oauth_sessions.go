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

	log "github.com/sirupsen/logrus"
)

const (
	oauthSessionTTL           = 30 * time.Minute
	oauthCompletedSessionTTL  = time.Minute
	maxOAuthStateLength       = 128
	maxOAuthNonceLength       = 128
	oauthSessionSourceBuiltin = "builtin"
	oauthSessionSourcePlugin  = "plugin"
	oauthNonceHeader          = "X-CPA-OAuth-Token"
)

var (
	errInvalidOAuthState      = errors.New("invalid oauth state")
	errUnsupportedOAuthFlow   = errors.New("unsupported oauth provider")
	errOAuthSessionNotPending = errors.New("oauth session is not pending")
	errOAuthSessionExists     = errors.New("oauth session already exists")
)

type oauthSession struct {
	Provider         string
	AuthDir          string
	Status           string
	Source           string
	Metadata         map[string]any
	Message          string
	NonceHash        string
	Initiator        [sha256.Size]byte
	CallbackAccepted bool
	Phase            string
	Polling          bool
	Saving           bool
	Completed        bool
	CreatedAt        time.Time
	ExpiresAt        time.Time
	cancel           context.CancelFunc
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
	completedTTL := min(ttl, oauthCompletedSessionTTL)
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

func (s *oauthSessionStore) register(state, provider, nonce, authDir string, initiator [sha256.Size]byte) {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
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
		AuthDir:   strings.TrimSpace(authDir),
		Source:    oauthSessionSourceBuiltin,
		NonceHash: hashOAuthNonce(nonce),
		Initiator: initiator,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
}

func (s *oauthSessionStore) registerPlugin(
	state,
	provider,
	nonce,
	authDir string,
	metadata map[string]any,
	initiator [sha256.Size]byte,
	registered ...*oauthSession,
) error {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	authDir = strings.TrimSpace(authDir)
	if state == "" || provider == "" || authDir == "" {
		return fmt.Errorf("%w: missing plugin OAuth session field", errInvalidOAuthState)
	}
	if errState := validateOAuthState(state); errState != nil {
		return errState
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	if _, exists := s.sessions[state]; exists {
		return errOAuthSessionExists
	}
	session := oauthSession{
		Provider:  provider,
		AuthDir:   authDir,
		Source:    oauthSessionSourcePlugin,
		Metadata:  cloneOAuthSessionMetadata(metadata),
		NonceHash: hashOAuthNonce(nonce),
		Initiator: initiator,
		CreatedAt: now,
		ExpiresAt: now.Add(s.ttl),
	}
	s.sessions[state] = session
	if len(registered) > 0 && registered[0] != nil {
		session.Metadata = cloneOAuthSessionMetadata(session.Metadata)
		*registered[0] = session
	}
	return nil
}

func (s *oauthSessionStore) setErrorIfPending(state, provider, message string, createdAt ...time.Time) bool {
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
	if !ok || len(createdAt) > 0 && !session.CreatedAt.Equal(createdAt[0]) || session.Completed || session.Saving ||
		session.Status != "" ||
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

func (s *oauthSessionStore) setError(state, message string, createdAt ...time.Time) {
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
	if !ok || len(createdAt) > 0 && !session.CreatedAt.Equal(createdAt[0]) || session.Completed {
		s.mu.Unlock()
		return
	}
	cancel := session.cancel
	session.cancel = nil
	session.Status = message
	session.Saving = false
	session.Phase = ""
	session.Polling = false
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (s *oauthSessionStore) complete(state string, createdAt ...time.Time) {
	state = strings.TrimSpace(state)
	if state == "" {
		return
	}
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || len(createdAt) > 0 && !session.CreatedAt.Equal(createdAt[0]) || session.Completed {
		s.mu.Unlock()
		return
	}
	cancel := session.cancel
	session.cancel = nil
	session.Status = ""
	session.Metadata = nil
	session.Saving = false
	session.Phase = ""
	session.Polling = false
	session.Completed = true
	session.ExpiresAt = now.Add(s.completedTTL)
	s.sessions[state] = session
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
}

func (s *oauthSessionStore) completeProvider(provider string) int {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return 0
	}
	var cancellations []context.CancelFunc
	now := time.Now()
	completed := 0
	s.mu.Lock()
	s.purgeExpiredLocked(now)
	for state, session := range s.sessions {
		if session.Completed || session.Source != oauthSessionSourceBuiltin ||
			!strings.EqualFold(session.Provider, provider) {
			continue
		}
		if session.cancel != nil {
			cancellations = append(cancellations, session.cancel)
		}
		session.cancel = nil
		session.Status = ""
		session.Metadata = nil
		session.Saving = false
		session.Phase = ""
		session.Polling = false
		session.Completed = true
		session.ExpiresAt = now.Add(s.completedTTL)
		s.sessions[state] = session
		completed++
	}
	s.mu.Unlock()
	for _, cancel := range cancellations {
		cancel()
	}
	return completed
}

func (s *oauthSessionStore) get(state string) (oauthSession, bool) {
	state = strings.TrimSpace(state)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	session.cancel = nil
	session.Metadata = cloneOAuthSessionMetadata(session.Metadata)
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

func (s *oauthSessionStore) attachCancel(
	state, provider string,
	cancel context.CancelFunc,
	createdAt ...time.Time,
) bool {
	if cancel == nil {
		return false
	}
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || len(createdAt) > 0 && !session.CreatedAt.Equal(createdAt[0]) || session.Completed || session.Saving ||
		session.Status != "" ||
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

func (s *oauthSessionStore) claimCallback(
	state, provider string,
	manual *oauthCallbackInitiator,
	authDir ...string,
) (string, bool, bool) {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.CallbackAccepted || session.Completed || session.Saving || session.Status != "" ||
		(provider != "" && !strings.EqualFold(session.Provider, provider)) ||
		(manual != nil && !session.acceptsManualCallback(*manual)) {
		return "", false, false
	}
	directoryBound := false
	if len(authDir) > 0 {
		requestedDir := strings.TrimSpace(authDir[0])
		if requestedDir == "" {
			return "", false, false
		}
		if session.AuthDir != "" {
			captured, errCaptured := filepath.Abs(session.AuthDir)
			requested, errRequested := filepath.Abs(requestedDir)
			if errCaptured != nil || errRequested != nil || captured != requested {
				return "", false, false
			}
		} else {
			session.AuthDir = requestedDir
			directoryBound = true
		}
	}
	session.CallbackAccepted = true
	session.ExpiresAt = now.Add(s.ttl)
	s.sessions[state] = session
	return session.AuthDir, directoryBound, true
}

func (s *oauthSessionStore) releaseCallback(state, provider string, releaseDirectory ...bool) {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	if state == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[state]
	if !ok || session.Completed || session.Saving || session.Status != "" ||
		(provider != "" && !strings.EqualFold(session.Provider, provider)) {
		return
	}
	session.CallbackAccepted = false
	if len(releaseDirectory) > 0 && releaseDirectory[0] {
		session.AuthDir = ""
	}
	s.sessions[state] = session
}

func (s *oauthSessionStore) callbackClaimActive(state, provider string) bool {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	return ok && session.CallbackAccepted && !session.Completed && !session.Saving && session.Status == "" &&
		(provider == "" || strings.EqualFold(session.Provider, provider))
}

func (s *oauthSessionStore) claimForSave(state, provider string, createdAt ...time.Time) bool {
	state = strings.TrimSpace(state)
	provider = strings.ToLower(strings.TrimSpace(provider))
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || len(createdAt) > 0 && !session.CreatedAt.Equal(createdAt[0]) || session.Completed || session.Saving ||
		session.Status != "" ||
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

func (s *oauthSessionStore) cancel(state string, createdAt ...time.Time) bool {
	state = strings.TrimSpace(state)
	if state == "" {
		return false
	}
	now := time.Now()

	s.mu.Lock()
	s.purgeExpiredLocked(now)
	session, ok := s.sessions[state]
	if !ok || session.Completed || session.Saving || session.Status != "" ||
		(len(createdAt) > 0 && !session.CreatedAt.Equal(createdAt[0])) {
		s.mu.Unlock()
		return false
	}
	delete(s.sessions, state)
	s.mu.Unlock()

	if session.cancel != nil {
		session.cancel()
	}
	if session.CallbackAccepted && strings.TrimSpace(session.AuthDir) != "" {
		callbackPath := filepath.Join(
			session.AuthDir,
			fmt.Sprintf(".oauth-%s-%s.oauth", session.Provider, state),
		)
		if errRemove := os.Remove(callbackPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Warn("failed to remove cancelled OAuth callback file")
		}
	}
	return true
}

func cloneOAuthSessionMetadata(metadata map[string]any) map[string]any {
	if len(metadata) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(metadata))
	for key, value := range metadata {
		cloned[key] = cloneOAuthSessionMetadataValue(value)
	}
	return cloned
}

func cloneOAuthSessionMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneOAuthSessionMetadata(typed)
	case []string:
		return append([]string(nil), typed...)
	case []any:
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneOAuthSessionMetadataValue(item)
		}
		return cloned
	default:
		return value
	}
}

var oauthSessions = newOAuthSessionStore(oauthSessionTTL)

func registerOAuthSession(state, provider, nonce string) {
	oauthSessions.register(state, provider, nonce, "", [sha256.Size]byte{})
}

func registerOAuthSessionForAuthDir(state, provider, nonce, authDir string, mutationContext ...context.Context) {
	var initiator [sha256.Size]byte
	if len(mutationContext) > 0 {
		initiator = ManagementSessionID(mutationContext[0])
	}
	oauthSessions.register(state, provider, nonce, authDir, initiator)
	if len(mutationContext) > 0 && isOAuthSessionPending(state, provider) {
		RecordMutationEffect(mutationContext[0], "task.created", "task")
	}
}

func registerPluginOAuthSession(
	state, provider, nonce, authDir string,
	metadata map[string]any,
	registered ...*oauthSession,
) error {
	return oauthSessions.registerPlugin(state, provider, nonce, authDir, metadata, [sha256.Size]byte{}, registered...)
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

func getOAuthSessionDetails(state string) (provider string, status string, completed bool, ok bool) {
	session, ok := oauthSessions.get(state)
	if !ok {
		return "", "", false, false
	}
	return session.Provider, session.Status, session.Completed, true
}

func getOAuthSessionIntegrationDetails(state string) (oauthSession, bool) {
	return oauthSessions.get(state)
}

func oauthSessionPhase(session oauthSession) string {
	switch {
	case session.Completed:
		return "success"
	case session.Phase != "":
		return session.Phase
	case session.Saving:
		return "saving"
	case session.CallbackAccepted:
		return "accepted"
	default:
		return "pending"
	}
}

func verifyOAuthNonce(state, nonce string) bool {
	session, ok := oauthSessions.get(state)
	return ok && session.verifyNonce(nonce)
}

func (session oauthSession) verifyNonce(nonce string) bool {
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
	case "kimi-ai", "kimi.ai":
		return "kimi-ai", nil
	case "meta", "muse":
		return "meta", nil
	case "xai", "x-ai", "x.ai", "grok":
		return "xai", nil
	case "devin", "cognition":
		return "devin", nil
	default:
		return "", errUnsupportedOAuthFlow
	}
}

func normalizePluginOAuthProvider(provider string) (string, error) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return "", errUnsupportedOAuthFlow
	}
	for _, r := range provider {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return "", errUnsupportedOAuthFlow
		}
	}
	return provider, nil
}

func normalizeOAuthCallbackProviderForPendingSession(provider, state string) (string, error) {
	session, ok := oauthSessions.get(state)
	if ok && session.Source == oauthSessionSourcePlugin {
		return normalizePluginOAuthProvider(provider)
	}
	return normalizeOAuthProvider(provider)
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
	canonicalProvider, err := normalizePluginOAuthProvider(provider)
	if err != nil {
		return "", err
	}
	if err := validateOAuthState(state); err != nil {
		return "", err
	}

	if strings.TrimSpace(code) == "" && strings.TrimSpace(errorMessage) == "" {
		return "", fmt.Errorf("oauth callback requires a code or error")
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
	if errMkdir := os.MkdirAll(authDir, 0o700); errMkdir != nil {
		return "", fmt.Errorf("create oauth callback directory: %w", errMkdir)
	}
	tmp, errCreate := os.CreateTemp(authDir, ".oauth-callback-*.tmp")
	if errCreate != nil {
		return "", fmt.Errorf("create oauth callback file: %w", errCreate)
	}
	tmpPath := tmp.Name()
	published := false
	defer func() {
		if published {
			return
		}
		if errRemove := os.Remove(tmpPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Warn("failed to remove temporary OAuth callback file")
		}
	}()
	if errChmod := tmp.Chmod(0o600); errChmod != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("secure oauth callback file: %w", errChmod)
	}
	if _, errWrite := tmp.Write(data); errWrite != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("write oauth callback file: %w", errWrite)
	}
	if errClose := tmp.Close(); errClose != nil {
		return "", fmt.Errorf("close oauth callback file: %w", errClose)
	}
	if errRename := os.Rename(tmpPath, filePath); errRename != nil {
		return "", fmt.Errorf("publish oauth callback file: %w", errRename)
	}
	published = true
	return filePath, nil
}

func WriteOAuthCallbackFileForPendingSession(
	provider string,
	state string,
	code string,
	errorMessage string,
	authDir ...string,
) (string, error) {
	return writePendingOAuthCallback(provider, state, code, errorMessage, nil, authDir...)
}

func writePendingOAuthCallback(
	provider, state, code, errorMessage string,
	manual *oauthCallbackInitiator,
	authDir ...string,
) (string, error) {
	canonicalProvider, err := normalizeOAuthCallbackProviderForPendingSession(provider, state)
	if err != nil {
		return "", err
	}
	sessionAuthDir, directoryBound, claimed := oauthSessions.claimCallback(state, canonicalProvider, manual, authDir...)
	if !claimed {
		return "", errOAuthSessionNotPending
	}
	if strings.TrimSpace(sessionAuthDir) == "" {
		oauthSessions.releaseCallback(state, canonicalProvider, directoryBound)
		return "", fmt.Errorf("oauth session auth dir is empty")
	}
	filePath, errWrite := writeOAuthCallbackFile(sessionAuthDir, canonicalProvider, state, code, errorMessage)
	if errWrite != nil {
		oauthSessions.releaseCallback(state, canonicalProvider, directoryBound)
		return "", errWrite
	}
	if !oauthSessions.callbackClaimActive(state, canonicalProvider) {
		if errRemove := os.Remove(filePath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
			log.WithError(errRemove).Warn("failed to remove cancelled OAuth callback file")
		}
		return "", errOAuthSessionNotPending
	}
	return filePath, nil
}

func (s *oauthSessionStore) setPhase(state, provider, phase string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(time.Now())
	session, ok := s.sessions[state]
	if !ok || session.Completed || session.Status != "" || !strings.EqualFold(session.Provider, provider) ||
		phase == "exchanging" && session.Saving {
		return false
	}
	session.Phase = phase
	s.sessions[state] = session
	return true
}

func (s *oauthSessionStore) claimPoll(state, provider string, createdAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purgeExpiredLocked(time.Now())
	session, ok := s.sessions[state]
	if !ok || !session.CreatedAt.Equal(createdAt) || session.Completed || session.Saving || session.Polling ||
		session.Status != "" ||
		!strings.EqualFold(session.Provider, provider) {
		return false
	}
	session.Polling = true
	session.Phase = "exchanging"
	s.sessions[state] = session
	return true
}

func (s *oauthSessionStore) releasePoll(state, provider, message string, createdAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[state]
	if !ok || !session.CreatedAt.Equal(createdAt) || !strings.EqualFold(session.Provider, provider) {
		return
	}
	session.Polling = false
	session.Message = message
	if !session.Saving && session.Phase == "exchanging" {
		session.Phase = ""
	}
	s.sessions[state] = session
}

type oauthSessionContextKey struct{}
type oauthSessionBinding struct {
	state, provider string
	store           *oauthSessionStore
}

func withOAuthSessionContext(ctx context.Context, state, provider string) context.Context {
	return context.WithValue(
		ctx,
		oauthSessionContextKey{},
		oauthSessionBinding{state: state, provider: provider, store: oauthSessions},
	)
}
func setOAuthSessionPhaseFromContext(ctx context.Context, phase string) {
	if binding, ok := ctx.Value(oauthSessionContextKey{}).(oauthSessionBinding); ok {
		binding.store.setPhase(binding.state, binding.provider, phase)
	}
}
