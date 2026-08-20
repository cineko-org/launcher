package centralclient

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	contracts "github.com/cineko-org/contracts/v3"
)

func TestResumedSessionPersistsRotatedRefreshToken(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	var persisted contracts.AuthExchangeResponse
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		releaseGeneration(writer, "7")
		switch request.URL.Path {
		case "/v1/auth/refresh":
			_ = json.NewEncoder(writer).Encode(contracts.AuthExchangeResponse{ // #nosec G117 -- test fixture.
				AccessToken: "new-access", ExpiresAt: now.Add(time.Hour), RefreshToken: "new-refresh",
				RefreshExpiresAt: now.Add(24 * time.Hour), User: contracts.ClientUser{ID: "user"},
			})
		case "/v1/client/bootstrap":
			if request.Header.Get("Authorization") != "Bearer new-access" {
				t.Errorf("bootstrap authorization = %q", request.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(writer).Encode(contracts.ClientBootstrap{User: contracts.ClientUser{ID: "user"}})
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	store, err := Resume(SessionConfig{
		BaseURL: server.URL, UserID: "user", AccessToken: "expired", ExpiresAt: now.Add(-time.Minute),
		RefreshToken: "old-refresh", RefreshExpiresAt: now.Add(time.Hour), HTTPClient: server.Client(),
		SessionChanged: func(session contracts.AuthExchangeResponse) error { persisted = session; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	if persisted.RefreshToken != "new-refresh" || store.Session().RefreshToken != "new-refresh" {
		t.Fatalf("rotated session was not persisted: callback=%+v store=%+v", persisted, store.Session())
	}
}

func TestRefreshPersistenceFailureKeepsPreviousSessionAndStopsRequest(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	var bootstrapCalls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		releaseGeneration(writer, "7")
		switch request.URL.Path {
		case "/v1/auth/refresh":
			_ = json.NewEncoder(writer).Encode(contracts.AuthExchangeResponse{ // #nosec G117 -- test fixture.
				AccessToken: "new-access", ExpiresAt: now.Add(time.Hour), RefreshToken: "new-refresh",
				RefreshExpiresAt: now.Add(24 * time.Hour), User: contracts.ClientUser{ID: "user"},
			})
		case "/v1/client/bootstrap":
			bootstrapCalls.Add(1)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	store, err := Resume(SessionConfig{
		BaseURL: server.URL, UserID: "user", AccessToken: "old-access", ExpiresAt: now.Add(-time.Minute),
		RefreshToken: "old-refresh", RefreshExpiresAt: now.Add(time.Hour), HTTPClient: server.Client(),
		SessionChanged: func(contracts.AuthExchangeResponse) error { return errors.New("disk unavailable") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateSession(t.Context()); err == nil {
		t.Fatal("refresh persistence failure was ignored")
	}
	if bootstrapCalls.Load() != 0 {
		t.Fatalf("original request continued after persistence failure: calls=%d", bootstrapCalls.Load())
	}
	current := store.Session()
	if current.AccessToken != "old-access" || current.RefreshToken != "old-refresh" {
		t.Fatalf("in-memory session changed after persistence failure: %+v", current)
	}
}

func releaseGeneration(writer http.ResponseWriter, generation string) {
	writer.Header().Set(contracts.ReleaseGenerationHeader, generation)
}

func TestCheckHealthRequiresReadyCentral(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		releaseGeneration(writer, "7")
		if request.URL.Path != "/health" || request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write([]byte(`{"status":"ready"}`))
	}))
	t.Cleanup(server.Close)
	if err := CheckHealth(t.Context(), server.URL, server.Client()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckHealthClassifiesUnavailableCentral(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		releaseGeneration(writer, "7")
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	client := server.Client()
	url := server.URL
	server.Close()
	if err := CheckHealth(t.Context(), url, client); !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("health error = %v", err)
	}
}

func TestOpenPINClassifiesAuthenticationFailures(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		releaseGeneration(writer, "7")
		if request.URL.Path != "/v1/auth/pin" {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":{"code":"unauthorized","message":"invalid PIN"}}`))
	}))
	client := server.Client()
	url := server.URL
	config := PINConfig{BaseURL: url, PIN: "000000", InstallationID: "installation", DeviceID: "device", HTTPClient: client}
	if _, err := OpenPIN(t.Context(), config); !errors.Is(err, ErrPINInvalid) {
		t.Fatalf("invalid PIN error = %v", err)
	}
	server.Close()
	if _, err := OpenPIN(t.Context(), config); !errors.Is(err, ErrServerUnavailable) {
		t.Fatalf("unavailable Central error = %v", err)
	}
}

func TestStoreRecordsMonotonicReleaseGeneration(t *testing.T) {
	t.Parallel()
	responses := []string{"2", "5", "3"}
	requestIndex := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		releaseGeneration(writer, responses[requestIndex])
		requestIndex++
		_, _ = writer.Write([]byte(`{"status":"ready"}`))
	}))
	t.Cleanup(server.Close)
	store, err := newStore(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range responses {
		var output struct {
			Status string `json:"status"`
		}
		if err := store.doRequest(t.Context(), http.MethodGet, "/health", "", nil, &output, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := store.ReleaseGeneration(); got != 5 {
		t.Fatalf("release generation = %d", got)
	}
}

func TestStoreRejectsMissingOrMalformedReleaseGeneration(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "not-a-number", "-1"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				if value != "" {
					releaseGeneration(writer, value)
				}
				_, _ = writer.Write([]byte(`{"status":"ready"}`))
			}))
			t.Cleanup(server.Close)
			store, err := newStore(server.URL, "", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			var output any
			if err := store.doRequest(t.Context(), http.MethodGet, "/health", "", nil, &output, nil); err == nil {
				t.Fatal("invalid release generation accepted")
			}
		})
	}
}

func TestResponseStatusClassifiesStaleRelease(t *testing.T) {
	t.Parallel()
	err := responseStatusError(http.StatusConflict, []byte(`{
		"error":{"code":"stale_release","message":"runtime release is no longer current"}
	}`))
	if !errors.Is(err, ErrReleaseChanged) {
		t.Fatalf("stale release error = %v", err)
	}
}
