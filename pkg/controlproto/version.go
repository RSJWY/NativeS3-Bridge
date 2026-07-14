package controlproto

import "fmt"

// NegotiateVersion resolves the protocol version to use for a connection given
// the peer's advertised version. The panel calls this on receiving hello; the
// negotiated version is the minimum of both sides' ProtocolVersion, provided it
// is within each side's compatibility floor.
//
// It returns an error when the versions are incompatible (peer too old for this
// build, or this build too old for the peer). The caller must send an error
// message and close the connection on failure rather than retrying blindly.
func NegotiateVersion(peerVersion int) (int, error) {
	if peerVersion <= 0 {
		return 0, fmt.Errorf("invalid peer protocol version %d", peerVersion)
	}
	// Peer is older than the oldest version we still support.
	if peerVersion < MinCompatibleVersion {
		return 0, fmt.Errorf("peer protocol version %d below minimum supported %d", peerVersion, MinCompatibleVersion)
	}
	// Peer is newer than us: it must be able to speak down to our version.
	// We cannot know the peer's MinCompatibleVersion here, so we negotiate the
	// lower of the two ProtocolVersions and let the peer reject if it cannot
	// speak that low. The negotiated version never exceeds what this build
	// understands.
	negotiated := peerVersion
	if negotiated > ProtocolVersion {
		negotiated = ProtocolVersion
	}
	return negotiated, nil
}

// IsCompatible reports whether the given negotiated version can be spoken by
// this build.
func IsCompatible(version int) bool {
	return version >= MinCompatibleVersion && version <= ProtocolVersion
}
