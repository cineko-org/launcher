package centralclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	contracts "github.com/cineko-org/contracts/v3"
)

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
