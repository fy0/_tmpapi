package basispoints

import "sync"

const (
	nativeCallCacheMax = 512
	attachmentCacheMax = 256
)

// State carries the per-credential-account runtime caches for the Basispoints
// channel: the run_officejs native-call replay cache (so a client-echoed
// function_call restores the exact native identity the upstream issued) and
// the sha256 -> openai_file_id attachment cache. The Python reference persists
// these to SQLite; process memory is sufficient here — a restart merely means
// replayed client calls get re-wrapped into fresh envelopes.
//
// The zero value is ready to use; a nil *State is also safe (caching disabled).
type State struct {
	mu          sync.Mutex
	native      map[string]map[string]any
	nativeOrder []string
	attach      map[string]string
	attachOrder []string
}

func NewState() *State {
	return &State{
		native: make(map[string]map[string]any),
		attach: make(map[string]string),
	}
}

// StoreNativeCall stores the original run_officejs item under its call_id
// for later replay (FIFO eviction at 512 entries). Nil receiver is a no-op.
func (st *State) StoreNativeCall(callID string, item map[string]any) {
	if st == nil || callID == "" {
		return
	}
	copied := cloneMap(item)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.native == nil {
		st.native = make(map[string]map[string]any)
	}
	if _, exists := st.native[callID]; exists {
		st.native[callID] = copied
		return
	}
	st.native[callID] = copied
	st.nativeOrder = append(st.nativeOrder, callID)
	for len(st.nativeOrder) > nativeCallCacheMax {
		oldest := st.nativeOrder[0]
		st.nativeOrder = st.nativeOrder[1:]
		delete(st.native, oldest)
	}
}

// NativeCall returns a copy of the remembered native call item, or nil.
func (st *State) NativeCall(callID string) map[string]any {
	if st == nil || callID == "" {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.native == nil {
		return nil
	}
	item, exists := st.native[callID]
	if !exists {
		return nil
	}
	return cloneMap(item)
}

// CachedAttachment returns the openai_file_id previously uploaded for a sha256
// digest, or "".
func (st *State) CachedAttachment(digest string) string {
	if st == nil || digest == "" {
		return ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.attach[digest]
}

// CacheAttachment records a sha256 -> openai_file_id mapping (LRU, 256).
func (st *State) CacheAttachment(digest, fileID string) {
	if st == nil || digest == "" || fileID == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.attach == nil {
		st.attach = make(map[string]string)
	}
	if _, exists := st.attach[digest]; exists {
		st.attach[digest] = fileID
		return
	}
	st.attach[digest] = fileID
	st.attachOrder = append(st.attachOrder, digest)
	for len(st.attachOrder) > attachmentCacheMax {
		oldest := st.attachOrder[0]
		st.attachOrder = st.attachOrder[1:]
		delete(st.attach, oldest)
	}
}
