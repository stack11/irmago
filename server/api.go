package server

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"

	"github.com/Sirupsen/logrus"
	"github.com/go-errors/errors"
	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/internal/fs"
)

var Logger *logrus.Logger = logrus.StandardLogger()

// Configuration contains configuration for the irmarequestor library and irmaserver.
type Configuration struct {
	// irma_configuration. If not given, this will be popupated using IrmaConfigurationPath.
	IrmaConfiguration *irma.Configuration `json:"-"`
	// Path to schemes to parse (only used if IrmaConfiguration is not given)
	IrmaConfigurationPath string `json:"irmaconf" mapstructure:"irmaconf"`
	// Path to writable dir to write cache to (only used if IrmaConfiguration is not given)
	CachePath string `json:"cachepath" mapstructure:"cachepath"`
	// Whether or not to download default IRMA schemes if the specified irma_configuration is empty
	DownloadDefaultSchemes bool
	// Path to issuer private keys to parse
	IssuerPrivateKeysPath string `json:"privatekeys" mapstructure:"privatekeys"`
	// Issuer private keys
	IssuerPrivateKeys map[irma.IssuerIdentifier]*gabi.PrivateKey `json:"-"`
	// URL at which the IRMA app can reach this server during sessions
	URL string `json:"url" mapstructure:"url"`
	// Logging
	Logger *logrus.Logger `json:"-"`
}

// SessionResult contains session information such as the session status, type, possible errors,
// and disclosed attributes or attribute-based signature if appropriate to the session type.
type SessionResult struct {
	Token       string
	Status      Status
	Type        irma.Action
	ProofStatus irma.ProofStatus
	Disclosed   []*irma.DisclosedAttribute
	Signature   *irma.SignedMessage
	Err         *irma.RemoteError
}

// Status is the status of an IRMA session.
type Status string

const (
	StatusInitialized Status = "INITIALIZED" // The session has been started and is waiting for the client
	StatusConnected   Status = "CONNECTED"   // The client has retrieved the session request, we wait for its response
	StatusCancelled   Status = "CANCELLED"   // The session is cancelled, possibly due to an error
	StatusDone        Status = "DONE"        // The session has completed successfully
	StatusTimeout     Status = "TIMEOUT"     // Session timed out
)

// RemoteError converts an error and an explaining message to an *irma.RemoteError.
func RemoteError(err Error, message string) *irma.RemoteError {
	stack := string(debug.Stack())
	Logger.Warnf("Session error: %d %s %s\n%s", err.Status, err.Type, message, stack)
	return &irma.RemoteError{
		Status:      err.Status,
		Description: err.Description,
		ErrorName:   string(err.Type),
		Message:     message,
		Stacktrace:  stack,
	}
}

// JsonResponse JSON-marshals the specified object or error
// and returns it along with a suitable HTTP status code
func JsonResponse(v interface{}, err *irma.RemoteError) (int, []byte) {
	msg := v
	status := http.StatusOK
	if err != nil {
		msg = err
		status = err.Status
	}
	b, e := json.Marshal(msg)
	if e != nil {
		Logger.Error("Failed to serialize response:", e.Error())
		return http.StatusInternalServerError, nil
	}
	Logger.Tracef("HTTP JSON response: %d %s", status, string(b))
	return status, b
}

// WriteError writes the specified error and explaining message as JSON to the http.ResponseWriter.
func WriteError(w http.ResponseWriter, err Error, msg string) {
	WriteResponse(w, nil, RemoteError(err, msg))
}

// WriteJson writes the specified object as JSON to the http.ResponseWriter.
func WriteJson(w http.ResponseWriter, object interface{}) {
	WriteResponse(w, object, nil)
}

// WriteResponse writes the specified object or error as JSON to the http.ResponseWriter.
func WriteResponse(w http.ResponseWriter, object interface{}, rerr *irma.RemoteError) {
	status, bts := JsonResponse(object, rerr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(bts)
}

// WriteString writes the specified string to the http.ResponseWriter.
func WriteString(w http.ResponseWriter, str string) {
	Logger.Trace("HTTP text/plain response: ", str)
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(str))
}

// ParseSessionRequest tries to parse the specified bytes as an
// disclosure request, a signature request, and an issuance request, in that order.
// Returns an error if none of the attempts work.
func ParseSessionRequest(bts []byte) (request irma.SessionRequest, err error) {
	request = &irma.DisclosureRequest{}
	if err = irma.UnmarshalValidate(bts, request); err == nil {
		return request, nil
	}
	request = &irma.SignatureRequest{}
	if err = irma.UnmarshalValidate(bts, request); err == nil {
		return request, nil
	}
	request = &irma.IssuanceRequest{}
	if err = irma.UnmarshalValidate(bts, request); err == nil {
		return request, nil
	}
	Logger.Warn("Failed to parse as session request: ", string(bts))
	return nil, errors.New("Invalid or disabled session type")
}

func LocalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			return ip.String(), nil
		}
	}
	return "", errors.New("No IP found")
}

func CachePath() (string, error) {
	candidates := make([]string, 0, 2)
	if runtime.GOOS != "windows" {
		candidates = append(candidates, filepath.Join("/var/tmp", "irma"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "irma"))
	path := firstWritablePath(candidates)
	if path == "" {
		return "", errors.New("No writable temporary directory found")
	}
	return path, nil
}

func firstWritablePath(paths []string) string {
	for _, path := range paths {
		if err := fs.EnsureDirectoryExists(path); err != nil {
			continue
		}
		return path
	}
	return ""
}

func Verbosity(level int) logrus.Level {
	switch {
	case level == 1:
		return logrus.DebugLevel
	case level > 1:
		return logrus.TraceLevel
	default:
		return logrus.InfoLevel
	}
}

func log(level logrus.Level, err error) error {
	writer := Logger.WriterLevel(level)
	if e, ok := err.(*errors.Error); ok && Logger.IsLevelEnabled(logrus.TraceLevel) {
		_, _ = writer.Write([]byte(e.ErrorStack()))
	} else {
		_, _ = writer.Write([]byte(fmt.Sprintf("%s %s", reflect.TypeOf(err).String(), err.Error())))
	}
	return err
}

func LogFatal(err error) error {
	return log(logrus.FatalLevel, err)
}

func LogError(err error) error {
	return log(logrus.ErrorLevel, err)
}

func LogWarning(err error) error {
	return log(logrus.WarnLevel, err)
}

func ToJson(o interface{}) string {
	bts, _ := json.Marshal(o)
	return string(bts)
}
