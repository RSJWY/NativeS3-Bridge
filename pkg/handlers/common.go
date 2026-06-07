package handlers

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"log/slog"
	"net/http"
)

const serverHeader = "NativeS3-Bridge"

type S3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
}

func WriteS3Error(w http.ResponseWriter, code string, httpStatus int, resource string) {
	requestID := ensureStandardHeaders(w)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(httpStatus)
	if err := xml.NewEncoder(w).Encode(S3Error{
		Code:      code,
		Message:   errorMessage(code),
		Resource:  resource,
		RequestID: requestID,
	}); err != nil {
		slog.Warn("write s3 error", "error", err)
	}
}

func WriteXML(w http.ResponseWriter, status int, v any) {
	ensureStandardHeaders(w)
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	if err := xml.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write xml response", "error", err)
	}
}

func SetStandardHeaders(w http.ResponseWriter) {
	ensureStandardHeaders(w)
}

func ensureStandardHeaders(w http.ResponseWriter) string {
	requestID := w.Header().Get("x-amz-request-id")
	if requestID == "" {
		requestID = randomRequestID()
		w.Header().Set("x-amz-request-id", requestID)
	}
	w.Header().Set("Server", serverHeader)
	return requestID
}

func randomRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf)
}

func errorMessage(code string) string {
	switch code {
	case "NoSuchKey":
		return "The specified key does not exist."
	case "NoSuchBucket":
		return "The specified bucket does not exist."
	case "InvalidArgument":
		return "Invalid argument."
	case "InvalidBucketName":
		return "The specified bucket is not valid."
	case "InvalidRange":
		return "The requested range is not satisfiable."
	case "MethodNotAllowed":
		return "The specified method is not allowed."
	case "AccessDenied":
		return "Access denied."
	case "InvalidAccessKeyId":
		return "The AWS access key ID you provided does not exist in our records."
	case "SignatureDoesNotMatch":
		return "The request signature we calculated does not match the signature you provided."
	case "RequestTimeTooSkewed":
		return "The difference between the request time and the server time is too large."
	case "QuotaExceeded":
		return "The requested object exceeds the credential quota."
	case "SlowDown":
		return "Please reduce your request rate."
	case "BucketNotEmpty":
		return "The bucket you tried to delete is not empty."
	case "BadDigest":
		return "The Content-MD5 you specified did not match what we received."
	case "InvalidDigest":
		return "The Content-MD5 you specified is not valid."
	case "MalformedXML":
		return "The XML you provided was not well-formed or did not validate."
	case "InvalidRequest":
		return "The request is not valid."
	case "NoSuchUpload":
		return "The specified multipart upload does not exist."
	case "InvalidPart":
		return "One or more of the specified parts could not be found or had an invalid ETag."
	case "InvalidPartOrder":
		return "The list of parts was not in ascending order."
	default:
		return "Internal server error."
	}
}
