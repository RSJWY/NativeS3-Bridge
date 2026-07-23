package panel

import "sync"

// The panel owns live node connections and pending imports in process memory,
// so one active panel process is the supported topology. Serialize draft
// mutations per node inside that process so multi-statement invariants (for
// example bucket existence checks and webhook duplicate checks) cannot race
// with another admin request or an explicit publish.
const nodeDraftLockStripes = 64

var nodeDraftLocks [nodeDraftLockStripes]sync.Mutex

func lockNodeDraft(nodeID uint) func() {
	lock := &nodeDraftLocks[nodeID%nodeDraftLockStripes]
	lock.Lock()
	return lock.Unlock
}
