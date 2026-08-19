package centralclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

const (
	maximumResponseBody = 8 << 20
	sessionRefreshSkew  = time.Minute
)

var (
	errUnauthorized      = errors.New("central session is unauthorized")
	ErrPINInvalid        = errors.New("central PIN is invalid")
	ErrPINRateLimited    = errors.New("central PIN authentication is rate limited")
	ErrReleaseChanged    = errors.New("central release generation changed")
	ErrServerUnavailable = errors.New("central server is unavailable")
)

type Config struct {
	BaseURL     string
	UserID      string
	AccessToken string
	HTTPClient  *http.Client
}

type PINConfig struct {
	BaseURL        string
	PIN            string
	InstallationID string
	DeviceID       string
	HTTPClient     *http.Client
}

type SessionConfig struct {
	BaseURL          string
	UserID           string
	AccessToken      string
	ExpiresAt        time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	HTTPClient       *http.Client
}

type Store struct {
	baseURL string
	userID  string
	client  *http.Client
	clock   func() time.Time

	authMu            sync.Mutex
	accessToken       string
	expiresAt         time.Time
	refreshToken      string
	refreshExpiresAt  time.Time
	releaseGeneration atomic.Int64
}

func Open(ctx context.Context, config Config) (*Store, error) {
	store, err := newStore(config.BaseURL, config.UserID, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	if store.userID == "" || strings.TrimSpace(config.AccessToken) == "" {
		return nil, errors.New("central user ID and access token are required")
	}
	var auth contracts.AuthExchangeResponse
	err = store.request(ctx, http.MethodPost, "/v1/auth/exchange", false, contracts.AuthExchangeRequest{
		UserID: store.userID, AccessToken: strings.TrimSpace(config.AccessToken),
	}, &auth, nil)
	if err != nil {
		return nil, fmt.Errorf("authenticate with Central: %w", err)
	}
	if err := store.acceptSession(auth); err != nil {
		return nil, errors.New("central returned an invalid Client session")
	}
	return store, nil
}

func OpenPIN(ctx context.Context, config PINConfig) (*Store, error) {
	store, err := newStore(config.BaseURL, "", config.HTTPClient)
	if err != nil {
		return nil, err
	}
	var auth contracts.AuthExchangeResponse
	err = store.request(ctx, http.MethodPost, "/v1/auth/pin", false, contracts.ClientPINExchangeRequest{
		PIN: config.PIN, InstallationID: config.InstallationID, DeviceID: config.DeviceID,
	}, &auth, nil)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			return nil, ErrPINInvalid
		}
		return nil, fmt.Errorf("authenticate PIN with Central: %w", err)
	}
	store.userID = strings.TrimSpace(auth.User.ID)
	if store.userID == "" || store.acceptSession(auth) != nil {
		return nil, errors.New("central returned an invalid PIN session")
	}
	return store, nil
}

func CheckHealth(ctx context.Context, baseURL string, client *http.Client) error {
	store, err := newStore(baseURL, "", client)
	if err != nil {
		return err
	}
	var health struct {
		Status string `json:"status"`
	}
	if err := store.doRequest(ctx, http.MethodGet, "/health", "", nil, &health, nil); err != nil {
		return fmt.Errorf("%w: %w", ErrServerUnavailable, err)
	}
	if health.Status != "ready" {
		return fmt.Errorf("%w: unexpected health status", ErrServerUnavailable)
	}
	return nil
}

func Resume(config SessionConfig) (*Store, error) {
	store, err := newStore(config.BaseURL, config.UserID, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	if store.userID == "" || strings.TrimSpace(config.AccessToken) == "" ||
		strings.TrimSpace(config.RefreshToken) == "" || config.ExpiresAt.IsZero() ||
		!config.RefreshExpiresAt.After(store.clock()) {
		return nil, errors.New("persisted Central session is invalid")
	}
	store.accessToken = config.AccessToken
	store.expiresAt = config.ExpiresAt
	store.refreshToken = config.RefreshToken
	store.refreshExpiresAt = config.RefreshExpiresAt
	return store, nil
}

func (store *Store) ValidateSession(ctx context.Context) error {
	var bootstrap contracts.ClientBootstrap
	if err := store.request(ctx, http.MethodGet, "/v1/client/bootstrap", true, nil, &bootstrap, nil); err != nil {
		return err
	}
	if bootstrap.User.ID != store.userID {
		return errors.New("central session user mismatch")
	}
	return nil
}

func (store *Store) Session() contracts.AuthExchangeResponse {
	store.authMu.Lock()
	defer store.authMu.Unlock()
	return contracts.AuthExchangeResponse{
		AccessToken: store.accessToken, ExpiresAt: store.expiresAt,
		RefreshToken: store.refreshToken, RefreshExpiresAt: store.refreshExpiresAt,
		User: contracts.ClientUser{ID: store.userID},
	}
}

func (store *Store) Close() error { return nil }

func (store *Store) Logout(ctx context.Context) error {
	return store.request(ctx, http.MethodPost, "/v1/auth/logout", true, nil, nil, nil)
}

func (store *Store) RegisterDevice(ctx context.Context, device contracts.ClientDevice) (contracts.ClientDevice, error) {
	var registered contracts.ClientDevice
	err := store.request(
		ctx, http.MethodPut, "/v1/devices/"+url.PathEscape(device.InstallationID), true, device, &registered, nil,
	)
	return registered, err
}

func (store *Store) CurrentRuntimeRelease(ctx context.Context, platform, arch string) (contracts.RuntimeRelease, error) {
	query := url.Values{"channel": {"stable"}, "platform": {platform}, "arch": {arch}}
	var release contracts.RuntimeRelease
	err := store.request(ctx, http.MethodGet, "/v1/releases/runtime/current?"+query.Encode(), true, nil, &release, nil)
	return release, err
}

func (store *Store) CurrentLauncherRelease(
	ctx context.Context,
	platform string,
	arch string,
) (contracts.LauncherRelease, error) {
	query := url.Values{"channel": {"stable"}, "platform": {platform}, "arch": {arch}}
	var release contracts.LauncherRelease
	err := store.request(ctx, http.MethodGet, "/v1/releases/launcher/current?"+query.Encode(), true, nil, &release, nil)
	return release, err
}

func (store *Store) ReleaseGeneration() int64 {
	return store.releaseGeneration.Load()
}

func (store *Store) IssueLaunchTicket(
	ctx context.Context,
	request contracts.LaunchTicketRequest,
) (contracts.LaunchTicketResponse, error) {
	var response contracts.LaunchTicketResponse
	err := store.request(ctx, http.MethodPost, "/v1/launch-tickets", true, request, &response, map[string]string{
		"Idempotency-Key": request.Nonce,
	})
	return response, err
}

func newStore(rawURL, userID string, client *http.Client) (*Store, error) {
	baseURL, err := validateBaseURL(rawURL)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return &Store{baseURL: baseURL, userID: strings.TrimSpace(userID), client: client, clock: time.Now}, nil
}

func (store *Store) request(
	ctx context.Context,
	method, path string,
	authenticated bool,
	input, output any,
	headers map[string]string,
) error {
	if !authenticated {
		return store.doRequest(ctx, method, path, "", input, output, headers)
	}
	token, err := store.sessionToken(ctx, false)
	if err != nil {
		return err
	}
	err = store.doRequest(ctx, method, path, token, input, output, headers)
	if !errors.Is(err, errUnauthorized) {
		return err
	}
	token, err = store.sessionToken(ctx, true)
	if err != nil {
		return err
	}
	return store.doRequest(ctx, method, path, token, input, output, headers)
}

func (store *Store) doRequest(
	ctx context.Context,
	method, path, token string,
	input, output any,
	headers map[string]string,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode Central request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, store.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create Central request: %w", err)
	}
	request.Header.Set(contracts.ProtocolHeader, strconv.Itoa(contracts.ProtocolVersion))
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := store.client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: send Central request: %w", ErrServerUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if err := store.recordReleaseGeneration(response.Header.Get(contracts.ReleaseGenerationHeader)); err != nil {
		return err
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseBody+1))
	if err != nil {
		return fmt.Errorf("read Central response: %w", err)
	}
	if len(contents) > maximumResponseBody {
		return errors.New("central response exceeds size limit")
	}
	if err := responseStatusError(response.StatusCode, contents); err != nil {
		return err
	}
	if output == nil {
		if len(bytes.TrimSpace(contents)) != 0 {
			return errors.New("central returned an unexpected response body")
		}
		return nil
	}
	if err := decodeJSON(contents, output); err != nil {
		return fmt.Errorf("decode Central response: %w", err)
	}
	return nil
}

func (store *Store) recordReleaseGeneration(value string) error {
	generation, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || generation < 0 {
		return errors.New("central returned an invalid release generation")
	}
	for {
		current := store.releaseGeneration.Load()
		if generation <= current || store.releaseGeneration.CompareAndSwap(current, generation) {
			return nil
		}
	}
}

func responseStatusError(status int, contents []byte) error {
	if status >= http.StatusOK && status < http.StatusMultipleChoices {
		return nil
	}
	if status >= http.StatusInternalServerError {
		return fmt.Errorf("%w: HTTP %d", ErrServerUnavailable, status)
	}
	return decodeAPIError(status, contents)
}

func (store *Store) sessionToken(ctx context.Context, forceRefresh bool) (string, error) {
	store.authMu.Lock()
	defer store.authMu.Unlock()
	now := store.clock()
	if !forceRefresh && store.accessToken != "" && store.expiresAt.After(now.Add(sessionRefreshSkew)) {
		return store.accessToken, nil
	}
	if store.refreshToken == "" || !store.refreshExpiresAt.After(now) {
		return "", errUnauthorized
	}
	var auth contracts.AuthExchangeResponse
	err := store.doRequest(ctx, http.MethodPost, "/v1/auth/refresh", "", contracts.AuthRefreshRequest{
		RefreshToken: store.refreshToken,
	}, &auth, nil)
	if err != nil {
		return "", fmt.Errorf("refresh Central session: %w", err)
	}
	if err := store.acceptSessionLocked(auth, now); err != nil {
		return "", err
	}
	return store.accessToken, nil
}

func (store *Store) acceptSession(auth contracts.AuthExchangeResponse) error {
	store.authMu.Lock()
	defer store.authMu.Unlock()
	return store.acceptSessionLocked(auth, store.clock())
}

func (store *Store) acceptSessionLocked(auth contracts.AuthExchangeResponse, now time.Time) error {
	if strings.TrimSpace(auth.AccessToken) == "" || strings.TrimSpace(auth.RefreshToken) == "" ||
		auth.User.ID != store.userID || !auth.ExpiresAt.After(now) || !auth.RefreshExpiresAt.After(auth.ExpiresAt) {
		return errors.New("invalid Central Client session")
	}
	store.accessToken = auth.AccessToken
	store.expiresAt = auth.ExpiresAt
	store.refreshToken = auth.RefreshToken
	store.refreshExpiresAt = auth.RefreshExpiresAt
	return nil
}

func decodeAPIError(status int, contents []byte) error {
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if decodeJSON(contents, &envelope) != nil || envelope.Error.Code == "" {
		return fmt.Errorf("central request failed with HTTP %d", status)
	}
	switch envelope.Error.Code {
	case "unauthorized":
		return fmt.Errorf("%w: %s", errUnauthorized, envelope.Error.Message)
	case "rate_limited":
		return fmt.Errorf("%w: %s", ErrPINRateLimited, envelope.Error.Message)
	case "stale_release":
		return fmt.Errorf("%w: %s", ErrReleaseChanged, envelope.Error.Message)
	default:
		return fmt.Errorf("central %s: %s", envelope.Error.Code, envelope.Error.Message)
	}
}

func decodeJSON(contents []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("response must contain one JSON value")
	}
	return nil
}

func validateBaseURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("central URL is invalid")
	}
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopbackHost(parsed.Hostname())) {
		return "", errors.New("central URL must use HTTPS outside loopback development")
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
