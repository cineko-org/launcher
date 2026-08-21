package centralclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	clientpb "github.com/cineko-org/contracts/gen/go/cineko/client"
	commonpb "github.com/cineko-org/contracts/gen/go/cineko/common"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestResumedSessionPersistsRotatedRefreshToken(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	var persisted *clientpb.AuthenticationResponse
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		releaseGeneration(writer, "7")
		switch request.URL.Path {
		case "/v1/auth/refresh":
			writeProtoJSON(t, writer, authenticationResponse(now, "new-access", "new-refresh"))
		case "/v1/client/bootstrap":
			if request.Header.Get("Authorization") != "Bearer new-access" {
				t.Errorf("bootstrap authorization = %q", request.Header.Get("Authorization"))
			}
			bootstrap := &clientpb.Bootstrap{}
			user := &clientpb.User{}
			user.SetId("user")
			bootstrap.SetUser(user)
			writeProtoJSON(t, writer, bootstrap)
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(server.Close)
	store, err := Resume(SessionConfig{
		BaseURL: server.URL, UserID: "user", AccessToken: "expired", ExpiresAt: now.Add(-time.Minute),
		RefreshToken: "old-refresh", RefreshExpiresAt: now.Add(time.Hour), HTTPClient: server.Client(),
		SessionChanged: func(session *clientpb.AuthenticationResponse) error { persisted = session; return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ValidateSession(t.Context()); err != nil {
		t.Fatal(err)
	}
	if persisted.GetRefreshToken() != "new-refresh" || store.Session().GetRefreshToken() != "new-refresh" {
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
			writeProtoJSON(t, writer, authenticationResponse(now, "new-access", "new-refresh"))
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
		SessionChanged: func(*clientpb.AuthenticationResponse) error { return errors.New("disk unavailable") },
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
	if current.GetAccessToken() != "old-access" || current.GetRefreshToken() != "old-refresh" {
		t.Fatalf("in-memory session changed after persistence failure: %+v", current)
	}
}

func releaseGeneration(writer http.ResponseWriter, generation string) {
	writer.Header().Set("X-Cineko-Release-Generation", generation)
}

func authenticationResponse(now time.Time, accessToken, refreshToken string) *clientpb.AuthenticationResponse {
	auth := &clientpb.AuthenticationResponse{}
	auth.SetAccessToken(accessToken)
	auth.SetExpiresAt(timestamppb.New(now.Add(time.Hour)))
	auth.SetRefreshToken(refreshToken)
	auth.SetRefreshExpiresAt(timestamppb.New(now.Add(24 * time.Hour)))
	user := &clientpb.User{}
	user.SetId("user")
	auth.SetUser(user)
	return auth
}

func writeProtoJSON(t *testing.T, writer http.ResponseWriter, message proto.Message) {
	t.Helper()
	contents, err := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(contents)
}

func TestCheckHealthRequiresReadyCentral(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		releaseGeneration(writer, "7")
		if request.URL.Path != "/health" || request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		health := &commonpb.ServiceHealth{}
		health.SetReady(&commonpb.Ready{})
		writeProtoJSON(t, writer, health)
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
		errorResponse := &commonpb.APIErrorResponse{}
		apiError := &commonpb.APIError{}
		apiError.SetCode("unauthorized")
		apiError.SetMessage("invalid PIN")
		apiError.SetRequestId("test-request")
		errorResponse.SetError(apiError)
		writer.WriteHeader(http.StatusUnauthorized)
		writeProtoJSON(t, writer, errorResponse)
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
		health := &commonpb.ServiceHealth{}
		health.SetReady(&commonpb.Ready{})
		writeProtoJSON(t, writer, health)
	}))
	t.Cleanup(server.Close)
	store, err := newStore(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range responses {
		output := &commonpb.ServiceHealth{}
		if err := store.doRequest(t.Context(), http.MethodGet, "/health", "", nil, output, nil); err != nil {
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
				health := &commonpb.ServiceHealth{}
				health.SetReady(&commonpb.Ready{})
				writeProtoJSON(t, writer, health)
			}))
			t.Cleanup(server.Close)
			store, err := newStore(server.URL, "", server.Client())
			if err != nil {
				t.Fatal(err)
			}
			output := &commonpb.ServiceHealth{}
			if err := store.doRequest(t.Context(), http.MethodGet, "/health", "", nil, output, nil); err == nil {
				t.Fatal("invalid release generation accepted")
			}
		})
	}
}

func TestResponseStatusClassifiesStaleRelease(t *testing.T) {
	t.Parallel()
	errorResponse := &commonpb.APIErrorResponse{}
	apiError := &commonpb.APIError{}
	apiError.SetCode("stale_release")
	apiError.SetMessage("runtime release is no longer current")
	apiError.SetRequestId("test-request")
	errorResponse.SetError(apiError)
	contents, marshalErr := (protojson.MarshalOptions{UseProtoNames: false}).Marshal(errorResponse)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	err := responseStatusError(http.StatusConflict, contents)
	if !errors.Is(err, ErrReleaseChanged) {
		t.Fatalf("stale release error = %v", err)
	}
}
