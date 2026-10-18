package telegramstore

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/tgdrive/teldrive/v2/internal/cache"
)

const (
	defaultUploadThreads                = 4
	telegramUploadPart                  = 512 * 1024
	telegramReadChunk                   = 1024 * 1024
	telegramReadAlign                   = 4 * 1024
	defaultTelegramReadBuffers          = 32
	defaultTelegramReadParallel         = 4
	defaultTelegramReadTimeout          = 30 * time.Second
	defaultTelegramReadAttempts         = 3
	defaultTelegramReadFailoverAccounts = 2
	deleteBatchSize                     = 100
)

// Runner owns Telegram authentication, client lifetime, bot selection, retry,
// rate-limit, and flood-wait middleware. The callback is invoked only while the
// underlying gotd client is running.
type Runner interface {
	Run(ctx context.Context, userID int64, operation Operation, fn func(context.Context, *tg.Client) error) error
}

// BotProvider resolves the upload bots that must be members of newly created
// storage channels. Returning an empty list is valid for user-only deployments.
type BotProvider interface {
	ChannelBots(ctx context.Context, userID int64, api *tg.Client) ([]tg.InputUserClass, error)
}

type GotdStorage struct {
	runner               Runner
	botProvider          BotProvider
	downloadPool         *DownloadClientPool
	downloadReadBuffers  int
	downloadReadParallel int
	globalCache          cache.Cacher
}

type GotdStorageOption func(*GotdStorage)

func WithBotProvider(provider BotProvider) GotdStorageOption {
	return func(storage *GotdStorage) { storage.botProvider = provider }
}

func WithDownloadClientPool(pool *DownloadClientPool) GotdStorageOption {
	return func(storage *GotdStorage) { storage.downloadPool = pool }
}

func WithDownloadReadBuffers(buffers int) GotdStorageOption {
	return func(storage *GotdStorage) {
		if buffers > 0 {
			storage.downloadReadBuffers = buffers
		}
	}
}

func WithDownloadReadParallel(parallel int) GotdStorageOption {
	return func(storage *GotdStorage) {
		if parallel > 0 {
			storage.downloadReadParallel = parallel
		}
	}
}

func NewGotdStorage(runner Runner, c cache.Cacher, options ...GotdStorageOption) *GotdStorage {
	storage := &GotdStorage{runner: runner, globalCache: c, downloadReadBuffers: defaultTelegramReadBuffers, downloadReadParallel: defaultTelegramReadParallel}
	for _, option := range options {
		if option != nil {
			option(storage)
		}
	}
	return storage
}

func (s *GotdStorage) Upload(ctx context.Context, request UploadRequest) (StoredPart, error) {
	if s.runner == nil || request.UserID <= 0 || request.ChannelID == 0 || request.Reader == nil || request.Size < 0 || strings.TrimSpace(request.Name) == "" {
		return StoredPart{}, ErrInvalidRequest
	}
	threads := request.Threads
	if threads <= 0 {
		threads = defaultUploadThreads
	}

	var stored StoredPart
	err := s.runUpload(ctx, request.UserID, threads, func(runCtx context.Context, api *tg.Client) error {
		channel, err := inputChannel(runCtx, api, request.ChannelID)
		if err != nil {
			return err
		}
		uploaded, err := uploader.NewUploader(api).
			WithThreads(threads).
			WithPartSize(telegramUploadPart).
			Upload(runCtx, uploader.NewUpload(request.Name, request.Reader, request.Size))
		if err != nil {
			return fmt.Errorf("upload Telegram document bytes: %w", err)
		}

		document := message.UploadedDocument(uploaded).Filename(request.Name).ForceFile(true)
		response, err := message.NewSender(api).
			To(&tg.InputPeerChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash}).
			Media(runCtx, document)
		if err != nil {
			return fmt.Errorf("publish Telegram document message: %w", err)
		}
		messageID, documentSize, err := uploadedMessage(response)
		if err != nil {
			return err
		}
		if documentSize != request.Size {
			return fmt.Errorf("%w: got %d, want %d", ErrSizeMismatch, documentSize, request.Size)
		}
		stored = StoredPart{ChannelID: request.ChannelID, MessageID: messageID, Size: documentSize}
		return nil
	})
	if err != nil {
		return StoredPart{}, err
	}
	if stored.MessageID == 0 {
		return StoredPart{}, ErrMessageNotFound
	}
	return stored, nil
}

func (s *GotdStorage) runUpload(ctx context.Context, userID int64, threads int, fn func(context.Context, *tg.Client) error) error {
	return runWithConnections(ctx, s.runner, userID, OperationUpload, threads, fn)
}

func (s *GotdStorage) Metadata(ctx context.Context, request MetadataRequest) (StoredPart, error) {
	if s.runner == nil || request.UserID <= 0 || request.ChannelID == 0 || request.MessageID < 0 {
		return StoredPart{}, ErrInvalidRequest
	}
	var stored StoredPart
	err := s.runner.Run(ctx, request.UserID, OperationDownload, func(runCtx context.Context, api *tg.Client) error {
		_, size, err := fetchDocumentLocation(runCtx, api, request.ChannelID, request.MessageID, s.globalCache)
		if err != nil {
			return err
		}
		stored = StoredPart{ChannelID: request.ChannelID, MessageID: request.MessageID, Size: size}
		return nil
	})
	if err != nil {
		return StoredPart{}, err
	}
	return stored, nil
}

func (s *GotdStorage) OpenRange(ctx context.Context, request RangeRequest) (io.ReadCloser, error) {
	if s.runner == nil || request.UserID <= 0 || request.ChannelID == 0 || request.MessageID <= 0 || request.Offset < 0 || request.Length < -1 {
		return nil, ErrInvalidRequest
	}
	streamCtx, cancel := context.WithCancel(ctx)
	reader := newTelegramRangeReader(streamCtx, cancel, s.downloadReadBuffers, s.downloadReadParallel, s.globalCache)
	go func() {
		err := runWithConnections(streamCtx, s.runner, request.UserID, OperationDownload, s.downloadReadParallel, func(runCtx context.Context, api *tg.Client) error {
			clientID, _ := ClientID(runCtx)
			location, documentSize, err := fetchDocumentLocation(runCtx, api, request.ChannelID, request.MessageID, s.globalCache)
			if err != nil {
				return err
			}
			return fillRangeWithLocation(runCtx, []rangeClient{{api: api, clientID: clientID}}, request, reader, location, documentSize)
		})
		reader.finish(err)
	}()
	return reader, nil
}

// OpenDownloadSessionMulti serves one download from several Telegram accounts.
// Without the warm client pool it degrades to a single-account session.
func (s *GotdStorage) OpenDownloadSessionMulti(ctx context.Context, userID int64, accounts int, preferredAccountID int64) (DownloadSession, error) {
	if s == nil || s.runner == nil || userID <= 0 {
		return nil, ErrInvalidRequest
	}
	if s.downloadPool != nil {
		return s.downloadPool.OpenDownloadSessionMulti(ctx, userID, accounts, preferredAccountID)
	}
	return s.OpenDownloadSession(ctx, userID)
}

// WarmDownload connects the pooled download client for an account ahead of the
// first player request. It is a no-op when the pool is disabled.
func (s *GotdStorage) WarmDownload(ctx context.Context, userID, accountID int64) error {
	if s == nil || s.downloadPool == nil || userID <= 0 {
		return nil
	}
	return s.downloadPool.Warm(ctx, userID, accountID)
}

// DownloadRangeWarmupAvailable reports whether the configured storage can warm
// Telegram file routes ahead of playback.
func (s *GotdStorage) DownloadRangeWarmupAvailable() bool {
	return s != nil && s.downloadPool != nil
}

// WarmDownloadRange opens the preferred account's file-DC route and fetches a
// small prefix before the player starts. The pooled client and account-scoped
// document location are then reused by the actual stream request.
func (s *GotdStorage) WarmDownloadRange(ctx context.Context, userID, accountID, channelID, messageID int64) error {
	if s == nil || s.downloadPool == nil || userID <= 0 || accountID <= 0 || channelID == 0 || messageID <= 0 {
		return nil
	}
	session, err := s.downloadPool.openWarmSession(ctx, userID, accountID)
	if err != nil {
		return err
	}
	defer session.Close()

	reader, err := session.OpenRange(ctx, RangeRequest{
		UserID: userID, ChannelID: channelID, MessageID: messageID, Offset: 0, Length: 4 * 1024,
	})
	if err != nil {
		return err
	}
	defer reader.Close()
	var firstByte [1]byte
	_, err = io.ReadFull(reader, firstByte[:])
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// rangeClient pairs a Telegram API client with the account ID behind it. File
// references are bound to the account that fetched them, so each client keeps
// its own cached document location.
type rangeClient struct {
	api      *tg.Client
	clientID int64
}

type rangeClientSource func(context.Context, map[int64]struct{}) ([]rangeClient, error)

func fillRangeWithLocation(ctx context.Context, clients []rangeClient, request RangeRequest, reader *telegramRangeReader, location *tg.InputDocumentFileLocation, documentSize int64) error {
	if request.Offset > documentSize {
		return io.EOF
	}
	remaining := request.Length
	if remaining < 0 || request.Offset+remaining > documentSize {
		remaining = documentSize - request.Offset
	}
	return reader.fill(ctx, clients, request, location, remaining)
}

type gotdDownloadSession struct {
	ctx                  context.Context
	cancel               context.CancelFunc
	ready                chan struct{}
	done                 chan struct{}
	api                  *tg.Client
	err                  error
	clientFn             func() (*tg.Client, error)
	clientSource         rangeClientSource
	closeFn              func() error
	close                sync.Once
	mu                   sync.Mutex
	downloadReadBuffers  int
	downloadReadParallel int
	clientID             int64
	clients              []sessionClient

	globalCache cache.Cacher
	stats       *streamStats
}

// sessionClient is one leased Telegram account inside a download session.
// close releases the pool entry and the account lease.
type sessionClient struct {
	api      *tg.Client
	clientID int64
	close    func() error
}

type cachedDocumentLocation struct {
	Location *tg.InputDocumentFileLocation `msgpack:"location"`
	Size     int64                         `msgpack:"size"`
}

func fetchDocumentLocation(ctx context.Context, api *tg.Client, channelID, messageID int64, c cache.Cacher) (*tg.InputDocumentFileLocation, int64, error) {
	clientID, ok := ClientID(ctx)
	if c == nil || !ok {
		return documentLocation(ctx, api, channelID, messageID)
	}
	gk := cache.Key("telegram", "document", clientID, channelID, messageID)
	result, err := cache.Fetch(ctx, c, gk, 4*time.Hour, func() (cachedDocumentLocation, error) {
		loc, size, err := documentLocation(ctx, api, channelID, messageID)
		if err != nil {
			return cachedDocumentLocation{}, err
		}
		return cachedDocumentLocation{Location: loc, Size: size}, nil
	})
	if err != nil {
		return nil, 0, err
	}
	return result.Location, result.Size, nil
}

func refreshDocumentLocation(ctx context.Context, api *tg.Client, channelID, messageID int64, c cache.Cacher) (*tg.InputDocumentFileLocation, int64, error) {
	clientID, ok := ClientID(ctx)
	if c != nil && ok {
		_ = c.Delete(ctx, cache.Key("telegram", "document", clientID, channelID, messageID))
	}
	return fetchDocumentLocation(ctx, api, channelID, messageID, c)
}
func (s *GotdStorage) OpenDownloadSession(ctx context.Context, userID int64) (DownloadSession, error) {
	if s == nil || s.runner == nil || userID <= 0 {
		return nil, ErrInvalidRequest
	}
	if s.downloadPool != nil {
		return s.downloadPool.OpenDownloadSession(ctx, userID)
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	session := &gotdDownloadSession{
		ctx: sessionCtx, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{}),
		downloadReadBuffers:  s.downloadReadBuffers,
		downloadReadParallel: s.downloadReadParallel,
		globalCache:          s.globalCache,
		stats:                newStreamStats(),
	}
	go func() {
		err := runWithConnections(sessionCtx, s.runner, userID, OperationDownload, s.downloadReadParallel, func(runCtx context.Context, api *tg.Client) error {
			session.mu.Lock()
			session.api = api
			session.clientID, _ = ClientID(runCtx)
			session.mu.Unlock()
			close(session.ready)
			<-runCtx.Done()
			return runCtx.Err()
		})
		session.mu.Lock()
		session.err = err
		session.mu.Unlock()
		close(session.done)
	}()
	select {
	case <-session.ready:
		return session, nil
	case <-session.done:
		session.mu.Lock()
		err := session.err
		session.mu.Unlock()
		cancel()
		if err == nil {
			err = ErrClientUnavailable
		}
		return nil, err
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

func (s *gotdDownloadSession) Metadata(ctx context.Context, request MetadataRequest) (StoredPart, error) {
	clients, err := s.rangeClients(ctx, nil)
	if err != nil {
		return StoredPart{}, err
	}
	primary := clients[0]
	_, size, err := fetchDocumentLocation(WithClientID(ctx, primary.clientID), primary.api, request.ChannelID, request.MessageID, s.globalCache)
	if err != nil {
		return StoredPart{}, err
	}
	return StoredPart{ChannelID: request.ChannelID, MessageID: request.MessageID, Size: size}, nil
}

func (s *gotdDownloadSession) OpenRange(ctx context.Context, request RangeRequest) (io.ReadCloser, error) {
	clients, err := s.rangeClients(ctx, nil)
	if err != nil {
		return nil, err
	}
	primary := clients[0]
	streamCtx, cancel := context.WithCancel(ctx)
	reader := newTelegramRangeReader(streamCtx, cancel, s.downloadReadBuffers, s.downloadReadParallel, s.globalCache)
	reader.stats = s.stats
	go func() {
		location, size, locationErr := fetchDocumentLocation(WithClientID(streamCtx, primary.clientID), primary.api, request.ChannelID, request.MessageID, s.globalCache)
		if locationErr != nil {
			reader.finish(locationErr)
			return
		}
		reader.finish(fillRangeWithClientSource(streamCtx, s.rangeClients, primary, request, reader, location, size))
	}()
	return reader, nil
}

func (s *gotdDownloadSession) rangeClients(ctx context.Context, excluded map[int64]struct{}) ([]rangeClient, error) {
	if s.clientSource != nil {
		return s.clientSource(ctx, excluded)
	}
	if len(s.clients) > 0 {
		clients := make([]rangeClient, 0, len(s.clients))
		for _, client := range s.clients {
			if client.api == nil {
				continue
			}
			if _, skip := excluded[client.clientID]; skip {
				continue
			}
			clients = append(clients, rangeClient{api: client.api, clientID: client.clientID})
		}
		if len(clients) == 0 {
			return nil, ErrClientUnavailable
		}
		return clients, nil
	}
	api, err := s.client()
	if err != nil {
		return nil, err
	}
	if _, skip := excluded[s.clientID]; skip {
		return nil, ErrClientUnavailable
	}
	return []rangeClient{{api: api, clientID: s.clientID}}, nil
}

func (s *gotdDownloadSession) client() (*tg.Client, error) {
	if s.clientFn != nil {
		return s.clientFn()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.api == nil {
		if len(s.clients) > 0 && s.clients[0].api != nil {
			return s.clients[0].api, nil
		}
		if s.err != nil {
			return nil, s.err
		}
		return nil, ErrClientUnavailable
	}
	return s.api, nil
}

// StreamStats reports per-account counters for this download session.
func (s *gotdDownloadSession) StreamStats() []AccountStreamStats {
	return s.stats.snapshot()
}

func (s *gotdDownloadSession) Close() error {
	var err error
	s.close.Do(func() {
		if s.closeFn != nil {
			err = s.closeFn()
			return
		}
		for _, client := range s.clients {
			if client.close != nil {
				err = errors.Join(err, client.close())
			}
		}
		if s.cancel != nil {
			s.cancel()
		}
		if s.done != nil {
			<-s.done
		}
	})
	return err
}

type telegramRangeReader struct {
	ctx        context.Context
	cancel     context.CancelFunc
	buffers    chan *telegramRangeBuffer
	done       chan struct{}
	cur        *telegramRangeBuffer
	readErr    error
	finishOnce sync.Once
	closeOnce  sync.Once
	mu         sync.Mutex
	parallel   int
	timeout    time.Duration
	attempts   int
	cache      cache.Cacher
	stats      *streamStats
}

type telegramRangeBuffer struct {
	buf []byte
	off int
}

type telegramReadPlan struct {
	offset int64
	limit  int
	skip   int
	length int
}

func newTelegramRangeReader(ctx context.Context, cancel context.CancelFunc, buffers, parallel int, c cache.Cacher) *telegramRangeReader {
	if buffers <= 0 {
		buffers = defaultTelegramReadBuffers
	}
	if parallel <= 0 {
		parallel = defaultTelegramReadParallel
	}
	return &telegramRangeReader{
		ctx: ctx, cancel: cancel, parallel: parallel, cache: c,
		timeout: defaultTelegramReadTimeout, attempts: defaultTelegramReadAttempts,
		buffers: make(chan *telegramRangeBuffer, buffers), done: make(chan struct{}),
	}
}

func (r *telegramRangeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.cur == nil || r.cur.empty() {
		select {
		case buf, ok := <-r.buffers:
			if !ok {
				r.mu.Lock()
				err := r.readErr
				r.mu.Unlock()
				if err != nil && !errors.Is(err, context.Canceled) {
					return 0, err
				}
				return 0, io.EOF
			}
			r.cur = buf
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		}
	}
	n := copy(p, r.cur.remaining())
	r.cur.off += n
	return n, nil
}

func (r *telegramRangeReader) Close() error {
	r.closeOnce.Do(func() { r.cancel() })
	select {
	case <-r.done:
	case <-r.ctx.Done():
	}
	return nil
}

func (r *telegramRangeReader) finish(err error) {
	r.finishOnce.Do(func() {
		r.mu.Lock()
		r.readErr = err
		r.mu.Unlock()
		close(r.buffers)
		close(r.done)
	})
}

func (r *telegramRangeReader) fill(ctx context.Context, clients []rangeClient, request RangeRequest, primary *tg.InputDocumentFileLocation, remaining int64) error {
	if len(clients) == 0 {
		return ErrInvalidRequest
	}
	return r.fillWithClientSource(ctx, fixedRangeClientSource(clients), clients[0], request, primary, remaining)
}

func fixedRangeClientSource(clients []rangeClient) rangeClientSource {
	clients = append([]rangeClient(nil), clients...)
	return func(ctx context.Context, excluded map[int64]struct{}) ([]rangeClient, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		available := make([]rangeClient, 0, len(clients))
		for _, client := range clients {
			if _, skip := excluded[client.clientID]; !skip {
				available = append(available, client)
			}
		}
		if len(available) == 0 {
			return nil, ErrClientUnavailable
		}
		return available, nil
	}
}

func fillRangeWithClientSource(ctx context.Context, source rangeClientSource, primaryClient rangeClient, request RangeRequest, reader *telegramRangeReader, location *tg.InputDocumentFileLocation, documentSize int64) error {
	if request.Offset > documentSize {
		return io.EOF
	}
	remaining := request.Length
	if remaining < 0 || request.Offset+remaining > documentSize {
		remaining = documentSize - request.Offset
	}
	return reader.fillWithClientSource(ctx, source, primaryClient, request, location, remaining)
}

func (r *telegramRangeReader) fillWithClientSource(ctx context.Context, source rangeClientSource, primaryClient rangeClient, request RangeRequest, primary *tg.InputDocumentFileLocation, remaining int64) error {
	type readResult struct {
		seq     int64
		payload []byte
		err     error
	}
	if source == nil || remaining < 0 {
		return ErrInvalidRequest
	}
	if remaining == 0 {
		return nil
	}

	fetchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	completed := make(chan readResult, r.parallel)

	nextOffset, nextRemaining := request.Offset, remaining
	var nextSeq int64
	var emitSeq int64
	active := 0
	var launchErr error
	launchNext := func() bool {
		if nextRemaining <= 0 || nextSeq-emitSeq >= int64(r.parallel) || launchErr != nil {
			return false
		}
		clients, err := source(fetchCtx, nil)
		if err != nil {
			launchErr = err
			return false
		}
		if len(clients) == 0 {
			launchErr = ErrClientUnavailable
			return false
		}
		plan := planTelegramReads(nextOffset, nextRemaining, 1)[0]
		seq := nextSeq
		nextSeq++
		nextOffset += int64(plan.length)
		nextRemaining -= int64(plan.length)
		active++
		// Rotate planned chunks over accounts that are ready now. Cold accounts
		// join subsequent chunks as soon as their pooled clients become ready.
		client := clients[seq%int64(len(clients))]
		go func() {
			payload, accountID, elapsed, err := r.fetchPlan(fetchCtx, source, primaryClient, primary, request, plan, client)
			result := readResult{seq: seq, payload: payload, err: err}
			if err == nil {
				r.stats.record(accountID, int64(len(payload)), elapsed)
				if slog.Default().Enabled(ctx, slog.LevelDebug) {
					slog.DebugContext(ctx, "telegram.download_chunk",
						"account_id", accountID,
						"offset", plan.offset,
						"bytes", len(payload),
						"elapsed_ms", elapsed.Milliseconds(),
						"mbps", chunkMBps(len(payload), elapsed),
					)
				}
			}
			select {
			case completed <- result:
			case <-fetchCtx.Done():
			}
		}()
		return true
	}

	ready := make(map[int64]readResult, r.parallel)
	for nextRemaining > 0 || active > 0 {
		for active < r.parallel && launchNext() {
		}
		if active == 0 {
			if launchErr != nil {
				return launchErr
			}
			continue
		}

		var result readResult
		select {
		case result = <-completed:
			active--
		case <-ctx.Done():
			return ctx.Err()
		}
		// Keep out-of-order errors in the same ordered window as successful
		// chunks. A speculative read several megabytes ahead must not discard
		// earlier chunks that are still usable by the player.
		ready[result.seq] = result

		for {
			next, ok := ready[emitSeq]
			if !ok {
				break
			}
			if next.err != nil {
				return next.err
			}
			select {
			case r.buffers <- &telegramRangeBuffer{buf: next.payload}:
				delete(ready, emitSeq)
				emitSeq++
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return nil
}

func (r *telegramRangeReader) fetchPlan(ctx context.Context, source rangeClientSource, primaryClient rangeClient, primary *tg.InputDocumentFileLocation, request RangeRequest, plan telegramReadPlan, first rangeClient) ([]byte, int64, time.Duration, error) {
	attempted := make(map[int64]struct{})
	var failures []error
	client := first

	for failover := 0; ; failover++ {
		attempted[client.clientID] = struct{}{}
		started := time.Now()
		location := primary
		var err error
		if client.clientID != primaryClient.clientID || client.api != primaryClient.api {
			location, _, err = fetchDocumentLocation(WithClientID(ctx, client.clientID), client.api, request.ChannelID, request.MessageID, r.cache)
				if err != nil {
					err = fmt.Errorf("resolve Telegram document for account %d: %w", client.clientID, err)
				}
		}
		var payload []byte
		if err == nil {
			payload, err = r.fetchPlanFromClient(ctx, client, location, request, plan)
		}
		if err == nil {
			return payload, client.clientID, time.Since(started), nil
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			return nil, 0, 0, errors.Join(failures...)
		}
		if !isRetryableTelegramChunkError(err) || failover >= defaultTelegramReadFailoverAccounts {
			return nil, 0, 0, errors.Join(failures...)
		}

		fromAccountID := client.clientID
		next, sourceErr := source(ctx, attempted)
		if sourceErr != nil || len(next) == 0 {
			if sourceErr == nil {
				sourceErr = ErrClientUnavailable
			}
			return nil, 0, 0, errors.Join(append(failures, sourceErr)...)
		}
		client = next[0]
		slog.WarnContext(ctx, "telegram.download_chunk_failover",
			"offset", plan.offset,
			"from_account_id", fromAccountID,
			"to_account_id", client.clientID,
			"error", err,
		)
	}
}

func (r *telegramRangeReader) fetchPlanFromClient(ctx context.Context, client rangeClient, location *tg.InputDocumentFileLocation, request RangeRequest, plan telegramReadPlan) ([]byte, error) {
	clientCtx := WithClientID(ctx, client.clientID)
	download := func(callCtx context.Context, loc *tg.InputDocumentFileLocation) (tg.UploadFileClass, error) {
		return client.api.UploadGetFile(callCtx, &tg.UploadGetFileRequest{
			Location: loc,
			Offset:   plan.offset,
			Limit:    plan.limit,
			Precise:  true,
		})
	}

	var response tg.UploadFileClass
	var err error
	for range r.attempts {
		attemptCtx, attemptCancel := context.WithTimeout(clientCtx, r.timeout)
		response, err = download(attemptCtx, location)
		if _, expired := tgerr.AsType(err, "FILE_REFERENCE_EXPIRED"); expired {
			refreshed, _, refreshErr := refreshDocumentLocation(clientCtx, client.api, request.ChannelID, request.MessageID, r.cache)
			if refreshErr != nil {
				err = refreshErr
			} else if refreshed != nil {
				location = refreshed
				response, err = download(attemptCtx, refreshed)
			}
		}
		attemptCancel()
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		if !errors.Is(err, context.DeadlineExceeded) && !isTransientTelegramError(err) {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("download Telegram document chunk for account %d at %d: %w", client.clientID, plan.offset, err)
	}
	file, ok := response.(*tg.UploadFile)
	if !ok {
		return nil, fmt.Errorf("unexpected Telegram download response %T", response)
	}
	end := plan.skip + plan.length
	if len(file.Bytes) < end {
		return nil, fmt.Errorf("download Telegram document chunk for account %d at %d: %w", client.clientID, plan.offset, io.ErrUnexpectedEOF)
	}
	return file.Bytes[plan.skip:end], nil
}

func isRetryableTelegramChunkError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.ErrUnexpectedEOF) || isTransientTelegramError(err) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "retryuntilack") || strings.Contains(message, "retry limit reached")
}

func chunkMBps(bytes int, elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 0
	}
	return float64(bytes) / elapsed.Seconds() / (1 << 20)
}

func planTelegramReads(offset, remaining int64, count int) []telegramReadPlan {
	plans := make([]telegramReadPlan, 0, count)
	for remaining > 0 && len(plans) < count {
		requestOffset := offset / telegramReadAlign * telegramReadAlign
		skip := int(offset - requestOffset)
		boundaryRemaining := telegramReadChunk - int(requestOffset%telegramReadChunk)
		maxLimit := telegramReadAlign
		for maxLimit*2 <= boundaryRemaining {
			maxLimit *= 2
		}
		length := int(min(remaining, int64(maxLimit-skip)))
		limit := telegramReadAlign
		for limit < skip+length {
			limit *= 2
		}
		plans = append(plans, telegramReadPlan{
			offset: requestOffset, limit: limit, skip: skip, length: length,
		})
		offset += int64(length)
		remaining -= int64(length)
	}
	return plans
}

func (b *telegramRangeBuffer) empty() bool {
	return b == nil || len(b.buf)-b.off <= 0
}

func (b *telegramRangeBuffer) remaining() []byte {
	return b.buf[b.off:]
}

func (s *GotdStorage) CopyPart(ctx context.Context, userID, sourceChannelID, sourceMessageID, destinationChannelID int64) (StoredPart, error) {
	if s.runner == nil || userID <= 0 || sourceChannelID == 0 || sourceMessageID <= 0 || destinationChannelID == 0 {
		return StoredPart{}, ErrInvalidRequest
	}
	var copied StoredPart
	err := s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		location, size, err := documentLocation(runCtx, api, sourceChannelID, sourceMessageID)
		if err != nil {
			return err
		}
		destination, err := inputChannel(runCtx, api, destinationChannelID)
		if err != nil {
			return err
		}
		var randomBytes [8]byte
		if _, err := cryptorand.Read(randomBytes[:]); err != nil {
			return fmt.Errorf("generate Telegram copy random id: %w", err)
		}
		response, err := api.MessagesSendMedia(runCtx, &tg.MessagesSendMediaRequest{
			Silent:   true,
			Peer:     &tg.InputPeerChannel{ChannelID: destination.ChannelID, AccessHash: destination.AccessHash},
			Media:    &tg.InputMediaDocument{ID: &tg.InputDocument{ID: location.ID, AccessHash: location.AccessHash, FileReference: location.FileReference}},
			RandomID: int64(binary.BigEndian.Uint64(randomBytes[:])),
		})
		if err != nil {
			return fmt.Errorf("copy Telegram document: %w", err)
		}
		messageID, copiedSize, err := uploadedMessage(response)
		if err != nil {
			return err
		}
		if copiedSize != size {
			return fmt.Errorf("%w: copied %d, source %d", ErrSizeMismatch, copiedSize, size)
		}
		copied = StoredPart{ChannelID: destinationChannelID, MessageID: messageID, Size: copiedSize}
		return nil
	})
	if err != nil {
		return StoredPart{}, err
	}
	return copied, nil
}

func (s *GotdStorage) DeleteMessages(ctx context.Context, userID, channelID int64, messageIDs []int64) error {
	if s.runner == nil || userID <= 0 || channelID == 0 {
		return ErrInvalidRequest
	}
	if len(messageIDs) == 0 {
		return nil
	}
	return s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		channel, err := inputChannel(runCtx, api, channelID)
		if errors.Is(err, ErrInvalidChannel) {
			return nil
		}
		if err != nil {
			return err
		}
		for start := 0; start < len(messageIDs); start += deleteBatchSize {
			end := min(start+deleteBatchSize, len(messageIDs))
			ids := make([]int, 0, end-start)
			for _, id := range messageIDs[start:end] {
				if id <= 0 {
					return ErrInvalidRequest
				}
				ids = append(ids, int(id))
			}
			if _, err := api.ChannelsDeleteMessages(runCtx, &tg.ChannelsDeleteMessagesRequest{Channel: channel, ID: ids}); err != nil {
				return fmt.Errorf("delete Telegram messages: %w", err)
			}
		}
		return nil
	})
}

func (s *GotdStorage) ListDocumentMessages(ctx context.Context, request ListDocumentMessagesRequest) (DocumentMessagePage, error) {
	if s.runner == nil || request.UserID <= 0 || request.ChannelID == 0 || request.BeforeID < 0 || request.Limit <= 0 || request.Limit > 100 {
		return DocumentMessagePage{}, ErrInvalidRequest
	}
	var page DocumentMessagePage
	err := s.runner.Run(ctx, request.UserID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		channel, err := inputChannel(runCtx, api, request.ChannelID)
		if err != nil {
			return err
		}
		result, err := api.MessagesGetHistory(runCtx, &tg.MessagesGetHistoryRequest{
			Peer:     &tg.InputPeerChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
			OffsetID: int(request.BeforeID), Limit: request.Limit,
		})
		if err != nil {
			return fmt.Errorf("list Telegram channel history: %w", err)
		}
		modified, ok := result.AsModified()
		if !ok {
			return errors.New("list Telegram channel history: unexpected unmodified response")
		}
		messages := modified.GetMessages()
		page.Exhausted = len(messages) < request.Limit
		for _, item := range messages {
			message, ok := item.(*tg.Message)
			if !ok {
				continue
			}
			if page.BeforeID == 0 || int64(message.ID) < page.BeforeID {
				page.BeforeID = int64(message.ID)
			}
			if document, ok := message.Media.(*tg.MessageMediaDocument); ok {
				page.Messages = append(page.Messages, documentMessage(int64(message.ID), message.Date, document))
			}
		}
		if len(messages) > 0 && page.BeforeID == 0 {
			return errors.New("list Telegram channel history: page has no usable message ID")
		}
		return nil
	})
	if err != nil {
		return DocumentMessagePage{}, err
	}
	return page, nil
}

// GetDocumentMessages resolves authoritative file metadata for specific
// messages so imported catalog rows match the underlying Telegram documents.
func (s *GotdStorage) GetDocumentMessages(ctx context.Context, request GetDocumentMessagesRequest) ([]DocumentMessage, error) {
	if s.runner == nil || request.UserID <= 0 || request.ChannelID == 0 || len(request.MessageIDs) == 0 || len(request.MessageIDs) > 100 {
		return nil, ErrInvalidRequest
	}
	ids := make([]tg.InputMessageClass, 0, len(request.MessageIDs))
	for _, id := range request.MessageIDs {
		if id <= 0 {
			return nil, ErrInvalidRequest
		}
		ids = append(ids, &tg.InputMessageID{ID: int(id)})
	}
	var messages []DocumentMessage
	err := s.runner.Run(ctx, request.UserID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		channel, err := inputChannel(runCtx, api, request.ChannelID)
		if err != nil {
			return err
		}
		result, err := api.ChannelsGetMessages(runCtx, &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash},
			ID:      ids,
		})
		if err != nil {
			// Fall back to the history scan when the channel API cannot fetch by ID.
			return err
		}
		modified, ok := result.AsModified()
		if !ok {
			return errors.New("get Telegram messages: unexpected unmodified response")
		}
		for _, item := range modified.GetMessages() {
			message, ok := item.(*tg.Message)
			if !ok || message.ID <= 0 {
				continue
			}
			document, ok := message.Media.(*tg.MessageMediaDocument)
			if !ok {
				continue
			}
			messages = append(messages, documentMessage(int64(message.ID), message.Date, document))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return messages, nil
}

func documentMessage(id int64, date int, media *tg.MessageMediaDocument) DocumentMessage {
	message := DocumentMessage{ID: id, CreatedAt: time.Unix(int64(date), 0).UTC()}
	document, ok := media.Document.(*tg.Document)
	if !ok || document == nil {
		return message
	}
	message.Size = document.Size
	message.MimeType = document.MimeType
	for _, attribute := range document.Attributes {
		if filename, ok := attribute.(*tg.DocumentAttributeFilename); ok {
			message.Name = strings.TrimSpace(filename.FileName)
		}
	}
	if message.Name == "" {
		extension := ""
		if extensions, err := mime.ExtensionsByType(document.MimeType); err == nil && len(extensions) > 0 {
			extension = extensions[0]
		}
		message.Name = fmt.Sprintf("telegram-%d%s", id, extension)
	}
	return message
}

func (s *GotdStorage) CreateChannel(ctx context.Context, userID int64, name string) (Channel, error) {
	if s.runner == nil || userID <= 0 || strings.TrimSpace(name) == "" {
		return Channel{}, ErrInvalidRequest
	}
	var created Channel
	err := s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		response, err := api.ChannelsCreateChannel(runCtx, &tg.ChannelsCreateChannelRequest{
			Title:     strings.TrimSpace(name),
			Broadcast: true,
		})
		if err != nil {
			return fmt.Errorf("create Telegram channel: %w", err)
		}
		updates, ok := response.(*tg.Updates)
		if !ok {
			return fmt.Errorf("unexpected Telegram channel response %T", response)
		}
		for _, chat := range updates.Chats {
			channel, ok := chat.(*tg.Channel)
			if !ok || !isBroadcastChannel(channel) {
				continue
			}
			if s.botProvider != nil {
				bots, botErr := s.botProvider.ChannelBots(runCtx, userID, api)
				if botErr != nil {
					_, _ = api.ChannelsDeleteChannel(runCtx, channel.AsInput())
					return fmt.Errorf("resolve Telegram upload bots: %w", botErr)
				}
				for _, bot := range bots {
					if adminErr := setBotAdmin(runCtx, api, channel.AsInput(), bot); adminErr != nil {
						_, _ = api.ChannelsDeleteChannel(runCtx, channel.AsInput())
						return fmt.Errorf("add Telegram upload bot as channel admin: %w", adminErr)
					}
				}
			}
			created = Channel{ID: channel.ID, Name: channel.Title}
			return nil
		}
		return ErrInvalidChannel
	})
	if err != nil {
		return Channel{}, err
	}
	return created, nil
}

func (s *GotdStorage) DeleteChannel(ctx context.Context, userID, channelID int64) error {
	if s.runner == nil || userID <= 0 || channelID == 0 {
		return ErrInvalidRequest
	}
	return s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		channel, err := fullChannel(runCtx, api, channelID)
		if errors.Is(err, ErrInvalidChannel) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := api.ChannelsDeleteChannel(runCtx, channel.AsInput()); err != nil {
			return fmt.Errorf("delete Telegram channel: %w", err)
		}
		return nil
	})
}

func (s *GotdStorage) InviteBot(ctx context.Context, userID, channelID int64, username string) error {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	if s.runner == nil || userID <= 0 || channelID == 0 || username == "" {
		return ErrInvalidRequest
	}
	return s.runner.Run(ctx, userID, OperationManage, func(runCtx context.Context, api *tg.Client) error {
		slog.InfoContext(runCtx, "Resolving Telegram channel for bot provisioning",
			"user_id", userID,
			"channel_id", channelID,
			"bot_username", username,
		)
		channel, err := fullChannel(runCtx, api, channelID)
		if err != nil {
			return err
		}
		if !isBroadcastChannel(channel) {
			slog.InfoContext(runCtx, "Skipping bot provisioning for non-broadcast chat", "user_id", userID, "channel_id", channelID)
			return nil
		}
		resolved, err := api.ContactsResolveUsername(runCtx, &tg.ContactsResolveUsernameRequest{Username: username})
		if err != nil {
			return fmt.Errorf("resolve Telegram bot %s: %w", username, err)
		}
		var bot tg.InputUserClass
		var botID int64
		for _, item := range resolved.Users {
			user, ok := item.(*tg.User)
			if ok && user.Bot && strings.EqualFold(user.Username, username) {
				bot = user.AsInput()
				botID = user.ID
				break
			}
		}
		if bot == nil {
			return fmt.Errorf("resolve Telegram bot %s: %w", username, ErrInvalidRequest)
		}
		slog.InfoContext(runCtx, "Promoting Telegram bot to channel admin",
			"user_id", userID,
			"channel_id", channelID,
			"bot_id", botID,
			"bot_username", username,
		)
		if err := setBotAdmin(runCtx, api, channel.AsInput(), bot); err != nil {
			return fmt.Errorf("add Telegram bot %s as admin in channel %d: %w", username, channelID, err)
		}
		slog.InfoContext(runCtx, "Telegram bot promoted to channel admin",
			"user_id", userID,
			"channel_id", channelID,
			"bot_id", botID,
			"bot_username", username,
		)
		return nil
	})
}

func setBotAdmin(ctx context.Context, api *tg.Client, channel tg.InputChannelClass, bot tg.InputUserClass) error {
	if api == nil || channel == nil || bot == nil {
		return ErrInvalidRequest
	}

	required := tg.ChatAdminRights{
		ChangeInfo:     true,
		PostMessages:   true,
		EditMessages:   true,
		DeleteMessages: true,
		BanUsers:       true,
		InviteUsers:    true,
		PinMessages:    true,
		ManageCall:     true,
		Other:          true,
		ManageTopics:   true,
	}

	_, err := api.ChannelsEditAdmin(ctx, &tg.ChannelsEditAdminRequest{
		Channel:     channel,
		UserID:      bot,
		AdminRights: required,
		Rank:        "bot",
	})
	return err
}

func inputChannel(ctx context.Context, api *tg.Client, channelID int64) (*tg.InputChannel, error) {
	channel, err := fullChannel(ctx, api, channelID)
	if err != nil {
		return nil, err
	}
	return channel.AsInput(), nil
}

func fullChannel(ctx context.Context, api *tg.Client, channelID int64) (*tg.Channel, error) {
	response, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: channelID}})
	if err != nil {
		return nil, fmt.Errorf("resolve Telegram channel: %w", err)
	}
	chats := response.GetChats()
	if len(chats) == 0 {
		return nil, ErrInvalidChannel
	}
	channel, ok := chats[0].(*tg.Channel)
	if !ok {
		return nil, fmt.Errorf("unexpected Telegram chat %T", chats[0])
	}
	return channel, nil
}

func uploadedMessage(response tg.UpdatesClass) (int64, int64, error) {
	updates, ok := response.(*tg.Updates)
	if !ok {
		return 0, 0, fmt.Errorf("unexpected Telegram upload response %T", response)
	}
	for _, update := range updates.Updates {
		channelMessage, ok := update.(*tg.UpdateNewChannelMessage)
		if !ok {
			continue
		}
		msg, ok := channelMessage.Message.(*tg.Message)
		if !ok {
			continue
		}
		document, ok := messageDocument(msg)
		if !ok {
			return 0, 0, ErrDocumentNotFound
		}
		return int64(msg.ID), document.Size, nil
	}
	return 0, 0, ErrMessageNotFound
}

func documentLocation(ctx context.Context, api *tg.Client, channelID, messageID int64) (*tg.InputDocumentFileLocation, int64, error) {
	channel, err := inputChannel(ctx, api, channelID)
	if err != nil {
		return nil, 0, err
	}
	response, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: channel,
		ID:      []tg.InputMessageClass{&tg.InputMessageID{ID: int(messageID)}},
	})
	if err != nil {
		return nil, 0, fmt.Errorf("get Telegram document message: %w", err)
	}
	modified, ok := response.AsModified()
	if !ok || len(modified.GetMessages()) == 0 {
		return nil, 0, ErrMessageNotFound
	}
	msg, ok := modified.GetMessages()[0].(*tg.Message)
	if !ok {
		return nil, 0, ErrMessageNotFound
	}
	document, ok := messageDocument(msg)
	if !ok {
		return nil, 0, ErrDocumentNotFound
	}
	return document.AsInputDocumentFileLocation(""), document.Size, nil
}

func messageDocument(msg *tg.Message) (*tg.Document, bool) {
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	document, ok := media.Document.(*tg.Document)
	return document, ok
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (r *cancelReadCloser) Close() error {
	r.cancel()
	return r.ReadCloser.Close()
}
