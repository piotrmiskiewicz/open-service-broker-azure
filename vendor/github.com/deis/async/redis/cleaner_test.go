package redis

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestDefaultCleanCleansDeadWorkers(t *testing.T) {
	e := getTestEngine()

	// Add some workers to the worker set, but do not add any heartbeats for these
	// workers. i.e. They should appear dead.
	const workerCount = 5
	workerSetName := getDisposableWorkerSetName()
	for range [workerCount]struct{}{} {
		err := e.redisClient.SAdd(workerSetName, getDisposableWorkerID()).Err()
		assert.Nil(t, err)
	}

	// Override the default cleanActiveTaskQueue function to just count how many
	// times it is invoked and then notify us when its been invoked workerCount
	// times.
	var cleanActiveTaskQueueCallCount int
	cleanActiveTaskQueueMutex := sync.Mutex{}
	cleanActiveTaskQueueDoneCh := make(chan struct{})
	e.cleanActiveTaskQueue = func(context.Context, string, string, string) error {
		cleanActiveTaskQueueMutex.Lock()
		defer cleanActiveTaskQueueMutex.Unlock()
		cleanActiveTaskQueueCallCount++
		if cleanActiveTaskQueueCallCount == workerCount {
			close(cleanActiveTaskQueueDoneCh)
		}
		return nil
	}

	// Override the default cleanWatchedTaskQueue function to just count how many
	// times it is invoked and then notify us when its been invoked workerCount
	// times.
	var cleanWatchedTaskQueueCallCount int
	cleanWatchedTaskQueueMutex := sync.Mutex{}
	cleanWatchedTaskQueueDoneCh := make(chan struct{})
	e.cleanWatchedTaskQueue = func(context.Context,
		string,
		string,
		string,
	) error {
		cleanWatchedTaskQueueMutex.Lock()
		defer cleanWatchedTaskQueueMutex.Unlock()
		cleanWatchedTaskQueueCallCount++
		if cleanWatchedTaskQueueCallCount == workerCount {
			close(cleanWatchedTaskQueueDoneCh)
		}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error)
	go func() {
		errCh <- e.defaultClean(
			ctx,
			workerSetName,
			getDisposableQueueName(),
			getDisposableQueueName(),
			time.Second,
		)
	}()

	cleanActiveTaskQueueTimer := time.NewTimer(10 * time.Second)
	defer cleanActiveTaskQueueTimer.Stop()

	cleanWatchedTaskQueueTimer := time.NewTimer(10 * time.Second)
	defer cleanWatchedTaskQueueTimer.Stop()

	// Wait for cleanActiveTaskQueue to have been called workerCount
	// times.
	select {
	case <-cleanActiveTaskQueueDoneCh:
	case err := <-errCh:
		assert.Failf(
			t,
			err.Error(),
			"received an unanticipated error",
		)
	case <-cleanActiveTaskQueueTimer.C:
		assert.Failf(
			t,
			"",
			"timed out waiting for cleanActiveTaskQueue to be invoked %d times",
			workerCount,
		)
	}

	// Wait for cleanWatchedTaskQueue to have been called workerCount
	// times.
	select {
	case <-cleanWatchedTaskQueueDoneCh:
	case err := <-errCh:
		assert.Failf(
			t,
			err.Error(),
			"received an unanticipated error",
		)
	case <-cleanWatchedTaskQueueTimer.C:
		assert.Failf(
			t,
			"",
			"timed out waiting for cleanWatchedTaskQueue to be invoked %d times",
			workerCount,
		)
	}

	cancel()

	// Assert that the error returned from defaultClean indicates that the
	// context was canceled
	select {
	case err := <-errCh:
		assert.Equal(t, ctx.Err(), err)
	// If the context isn't canceled, move on and fail
	case <-time.After(time.Second):
		assert.Fail(
			t,
			"a context canceled error should have been returned, but wasn't",
		)
	}
}

func TestDefaultCleanDoesNotCleanLiveWorkers(t *testing.T) {
	e := getTestEngine()

	// Add a worker to the worker set. Also add a heartbeat so this worker appears
	// to be alive.
	workerSetName := getDisposableWorkerSetName()
	workerID := getDisposableWorkerID()
	err := e.redisClient.SAdd(workerSetName, workerID).Err()
	assert.Nil(t, err)
	err = e.redisClient.Set(getHeartbeatKey(workerID), aliveIndicator, 0).Err()
	assert.Nil(t, err)

	// Override the default cleanActiveTaskQueue function to just count how many
	// times it is invoked
	var cleanActiveTaskQueueCallCount int
	e.cleanActiveTaskQueue = func(context.Context, string, string, string) error {
		cleanActiveTaskQueueCallCount++
		return nil
	}

	// Override the default cleanWatchedTaskQueue function to just count how many
	// times it is invoked
	var cleanWatchedTaskQueueCallCount int
	e.cleanWatchedTaskQueue = func(
		context.Context,
		string,
		string,
		string,
	) error {
		cleanWatchedTaskQueueCallCount++
		return nil
	}

	// Under nominal conditions, defaultClean could block for a very long time,
	// unless the context it is passed is canceled. Use a context that will cancel
	// itself after 2 seconds to make defaultClean STOP working so we can then
	// examine what it accomplished.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Call defaultClean in a goroutine. If it never unblocks, as we hope it does,
	// we don't want the test to stall.
	errCh := make(chan error)
	go func() {
		errCh <- e.defaultClean(
			ctx,
			workerSetName,
			getDisposableQueueName(),
			getDisposableQueueName(),
			time.Second,
		)
	}()

	// Assert that the error returned from defaultClean indicates that the
	// context was canceled
	select {
	case err := <-errCh:
		assert.Equal(t, ctx.Err(), err)
	case <-time.After(time.Second * 3):
		assert.Fail(
			t,
			"a context canceled error should have been returned, but wasn't",
		)
	}

	// Assert neither cleanActiveTaskQueue and cleanWatchedTaskQueue were ever
	// invoked
	assert.Equal(t, 0, cleanActiveTaskQueueCallCount)
	assert.Equal(t, 0, cleanWatchedTaskQueueCallCount)
}

func TestDefaultCleanWorkerQueue(t *testing.T) {
	e := getTestEngine()

	sourceQueueName := getDisposableQueueName()
	destinationQueueName := getDisposableQueueName()

	const taskCount int64 = 5
	for range [taskCount]struct{}{} {
		// Put some dummy tasks onto the source queue
		err := e.redisClient.LPush(sourceQueueName, "foo").Err()
		assert.Nil(t, err)
	}

	// Assert that the source queue is precisely taskCount deep
	sourceQueueDepth, err := e.redisClient.LLen(sourceQueueName).Result()
	assert.Nil(t, err)
	assert.Equal(t, taskCount, sourceQueueDepth)

	// Assert that the destination queue starts out empty
	destinationQueueDepth, err := e.redisClient.LLen(destinationQueueName).Result()
	assert.Nil(t, err)
	assert.Empty(t, destinationQueueDepth)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err = e.defaultCleanWorkerQueue(
		ctx,
		getDisposableWorkerID(),
		sourceQueueName,
		destinationQueueName,
	)
	assert.Nil(t, err)

	// Assert that the source queue has been drained
	sourceQueueDepth, err = e.redisClient.LLen(sourceQueueName).Result()
	assert.Nil(t, err)
	assert.Empty(t, sourceQueueDepth)

	// Assert that the destination queue now has precisely taskCount tasks
	destinationQueueDepth, err = e.redisClient.LLen(destinationQueueName).Result()
	assert.Nil(t, err)
	assert.Equal(t, taskCount, destinationQueueDepth)
}

func TestDefaultCleanWorkerQueueRespondsToCanceledContext(t *testing.T) {
	e := getTestEngine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel before we even call defaultCleanWorkerQueue. In this case, its
	// the only way we can guarantee the function won't return before we have
	// a chance to cancel the context.
	cancel()

	err := e.defaultCleanWorkerQueue(
		ctx,
		getDisposableWorkerID(),
		getDisposableQueueName(),
		getDisposableQueueName(),
	)

	// Assert that the error returned indicates that the context was canceled
	assert.Equal(t, ctx.Err(), err)
}
