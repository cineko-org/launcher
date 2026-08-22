package centralclient

import (
	"bytes"
	"context"
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

	clientpb "github.com/cineko-org/contracts/v3/gen/go/cineko/client"
	commonpb "github.com/cineko-org/contracts/v3/gen/go/cineko/common"
	releasepb "github.com/cineko-org/contracts/v3/gen/go/cineko/release"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	BaseURL        string
	UserID         string
	AccessToken    string
	HTTPClient     *http.Client
	SessionChanged func(*clientpb.AuthenticationResponse) error
}

type PINConfig struct {
	BaseURL        string
	PIN            string
	InstallationID string
	DeviceID       string
	HTTPClient     *http.Client
	SessionChanged func(*clientpb.AuthenticationResponse) error
}

type SessionConfig struct {
	BaseURL          string
	UserID           string
	AccessToken      string
	ExpiresAt        time.Time
	RefreshToken     string
	RefreshExpiresAt time.Time
	HTTPClient       *http.Client
	SessionChanged   func(*clientpb.AuthenticationResponse) error
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
	sessionChanged    func(*clientpb.AuthenticationResponse) error
	releaseGeneration atomic.Int64
}

func Open(ctx context.Context, config Config) (*Store, error) {
	store, err := newStore(config.BaseURL, config.UserID, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	store.sessionChanged = config.SessionChanged
	if store.userID == "" || strings.TrimSpace(config.AccessToken) == "" {
		return nil, errors.New("central user ID and access token are required")
	}
	input := &clientpb.TokenExchangeRequest{}
	input.SetUserId(store.userID)
	input.SetAccessToken(strings.TrimSpace(config.AccessToken))
	auth := &clientpb.AuthenticationResponse{}
	err = store.request(ctx, http.MethodPost, "/v1/auth/exchange", false, input, auth, nil)
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
	store.sessionChanged = config.SessionChanged
	input := &clientpb.PinExchangeRequest{}
	input.SetPin(config.PIN)
	input.SetInstallationId(config.InstallationID)
	input.SetDeviceId(config.DeviceID)
	auth := &clientpb.AuthenticationResponse{}
	err = store.request(ctx, http.MethodPost, "/v1/auth/pin", false, input, auth, nil)
	if err != nil {
		if errors.Is(err, errUnauthorized) {
			return nil, ErrPINInvalid
		}
		return nil, fmt.Errorf("authenticate PIN with Central: %w", err)
	}
	store.userID = strings.TrimSpace(auth.GetUser().GetId())
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
	health := &commonpb.ServiceHealth{}
	if err := store.doRequest(ctx, http.MethodGet, "/health", "", nil, health, nil); err != nil {
		return fmt.Errorf("%w: %w", ErrServerUnavailable, err)
	}
	if health.GetReady() == nil {
		return fmt.Errorf("%w: unexpected health status", ErrServerUnavailable)
	}
	return nil
}

func Resume(config SessionConfig) (*Store, error) {
	store, err := newStore(config.BaseURL, config.UserID, config.HTTPClient)
	if err != nil {
		return nil, err
	}
	store.sessionChanged = config.SessionChanged
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
	bootstrap := &clientpb.Bootstrap{}
	if err := store.request(ctx, http.MethodGet, "/v1/client/bootstrap", true, nil, bootstrap, nil); err != nil {
		return err
	}
	if bootstrap.GetUser().GetId() != store.userID {
		return errors.New("central session user mismatch")
	}
	return nil
}

func (store *Store) Session() *clientpb.AuthenticationResponse {
	store.authMu.Lock()
	defer store.authMu.Unlock()
	auth := &clientpb.AuthenticationResponse{}
	auth.SetAccessToken(store.accessToken)
	auth.SetExpiresAt(timestamppb.New(store.expiresAt))
	auth.SetRefreshToken(store.refreshToken)
	auth.SetRefreshExpiresAt(timestamppb.New(store.refreshExpiresAt))
	user := &clientpb.User{}
	user.SetId(store.userID)
	auth.SetUser(user)
	return auth
}

func (store *Store) Close() error { return nil }

func (store *Store) Logout(ctx context.Context) error {
	return store.request(ctx, http.MethodPost, "/v1/auth/logout", true, nil, nil, nil)
}

func (store *Store) RegisterDevice(ctx context.Context, device *clientpb.Device) (*clientpb.Device, error) {
	registered := &clientpb.Device{}
	err := store.request(
		ctx, http.MethodPut, "/v1/devices/"+url.PathEscape(device.GetInstallationId()), true, device, registered, nil,
	)
	return registered, err
}

func (store *Store) CurrentRuntimeRelease(ctx context.Context, platform, arch string) (*releasepb.RuntimeRelease, error) {
	query := url.Values{"channel": {"stable"}, "platform": {platform}, "arch": {arch}}
	release := &releasepb.RuntimeRelease{}
	err := store.request(ctx, http.MethodGet, "/v1/releases/runtime/current?"+query.Encode(), true, nil, release, nil)
	return release, err
}

func (store *Store) CurrentLauncherRelease(
	ctx context.Context,
	platform string,
	arch string,
) (*releasepb.LauncherRelease, error) {
	query := url.Values{"channel": {"stable"}, "platform": {platform}, "arch": {arch}}
	release := &releasepb.LauncherRelease{}
	err := store.request(ctx, http.MethodGet, "/v1/releases/launcher/current?"+query.Encode(), true, nil, release, nil)
	return release, err
}

func (store *Store) ReleaseGeneration() int64 {
	return store.releaseGeneration.Load()
}

func (store *Store) IssueLaunchTicket(
	ctx context.Context,
	request *clientpb.LaunchTicketRequest,
) (*clientpb.LaunchTicketResponse, error) {
	response := &clientpb.LaunchTicketResponse{}
	err := store.request(ctx, http.MethodPost, "/v1/launch-tickets", true, request, response, map[string]string{
		"Idempotency-Key": request.GetNonce(),
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
	input, output proto.Message,
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
	input, output proto.Message,
	headers map[string]string,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(input)
		if err != nil {
			return fmt.Errorf("encode Central request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, store.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("create Central request: %w", err)
	}
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
	if err := store.recordReleaseGeneration(response.Header.Get("X-Cineko-Release-Generation")); err != nil {
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
	if err := (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(contents, output); err != nil {
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
	input := &clientpb.TokenRefreshRequest{}
	input.SetRefreshToken(store.refreshToken)
	auth := &clientpb.AuthenticationResponse{}
	err := store.doRequest(ctx, http.MethodPost, "/v1/auth/refresh", "", input, auth, nil)
	if err != nil {
		return "", fmt.Errorf("refresh Central session: %w", err)
	}
	if err := store.acceptSessionLocked(auth, now); err != nil {
		return "", err
	}
	return store.accessToken, nil
}

func (store *Store) acceptSession(auth *clientpb.AuthenticationResponse) error {
	store.authMu.Lock()
	defer store.authMu.Unlock()
	return store.acceptSessionLocked(auth, store.clock())
}

func (store *Store) acceptSessionLocked(auth *clientpb.AuthenticationResponse, now time.Time) error {
	if auth == nil || strings.TrimSpace(auth.GetAccessToken()) == "" || strings.TrimSpace(auth.GetRefreshToken()) == "" ||
		auth.GetUser().GetId() != store.userID || auth.GetExpiresAt() == nil || auth.GetRefreshExpiresAt() == nil ||
		!auth.GetExpiresAt().AsTime().After(now) || !auth.GetRefreshExpiresAt().AsTime().After(auth.GetExpiresAt().AsTime()) {
		return errors.New("invalid Central Client session")
	}
	if store.sessionChanged != nil {
		if err := store.sessionChanged(auth); err != nil {
			return fmt.Errorf("persist refreshed Central session: %w", err)
		}
	}
	store.accessToken = auth.GetAccessToken()
	store.expiresAt = auth.GetExpiresAt().AsTime()
	store.refreshToken = auth.GetRefreshToken()
	store.refreshExpiresAt = auth.GetRefreshExpiresAt().AsTime()
	return nil
}

func decodeAPIError(status int, contents []byte) error {
	envelope := &commonpb.APIErrorResponse{}
	if (protojson.UnmarshalOptions{DiscardUnknown: false}).Unmarshal(contents, envelope) != nil || envelope.GetError().GetCode() == "" {
		return fmt.Errorf("central request failed with HTTP %d", status)
	}
	switch envelope.GetError().GetCode() {
	case "unauthorized":
		return fmt.Errorf("%w: %s", errUnauthorized, envelope.GetError().GetMessage())
	case "rate_limited":
		return fmt.Errorf("%w: %s", ErrPINRateLimited, envelope.GetError().GetMessage())
	case "stale_release":
		return fmt.Errorf("%w: %s", ErrReleaseChanged, envelope.GetError().GetMessage())
	default:
		return fmt.Errorf("central %s: %s", envelope.GetError().GetCode(), envelope.GetError().GetMessage())
	}
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
