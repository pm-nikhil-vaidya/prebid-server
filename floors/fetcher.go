package floors

import (
	"container/heap"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/alitto/pond"
	validator "github.com/asaskevich/govalidator"
	"github.com/coocood/freecache"
	"github.com/golang/glog"
	"github.com/prebid/prebid-server/config"
	"github.com/prebid/prebid-server/metrics"
	"github.com/prebid/prebid-server/openrtb_ext"
	"github.com/prebid/prebid-server/util/timeutil"
)

var refetchCheckInterval = 300

type FetchInfo struct {
	config.AccountFloorFetch
	FetchTime      int64
	RefetchRequest bool
}

type WorkerPool interface {
	TrySubmit(task func()) bool
	Stop()
}

type FloorFetcher interface {
	Fetch(configs config.AccountPriceFloors) (*openrtb_ext.PriceFloorRules, string)
}

type PriceFloorFetcher struct {
	pool            WorkerPool            // Goroutines worker pool
	fetchQueue      FetchQueue            // Priority Queue to fetch floor data
	fetchInProgress map[string]bool       // Map of URL with fetch status
	configReceiver  chan FetchInfo        // Channel which recieves URLs to be fetched
	done            chan struct{}         // Channel to close fetcher
	cache           *freecache.Cache      // cache
	httpClient      *http.Client          // http client to fetch data from url
	time            timeutil.Time         // time interface to record request timings
	metricEngine    metrics.MetricsEngine // Records malfunctions in dynamic fetch
}

type FetchQueue []*FetchInfo

func (fq FetchQueue) Len() int {
	return len(fq)
}

func (fq FetchQueue) Less(i, j int) bool {
	return fq[i].FetchTime < fq[j].FetchTime
}

func (fq FetchQueue) Swap(i, j int) {
	fq[i], fq[j] = fq[j], fq[i]
}

func (fq *FetchQueue) Push(element interface{}) {
	fetchInfo := element.(*FetchInfo)
	*fq = append(*fq, fetchInfo)
}

func (fq *FetchQueue) Pop() interface{} {
	old := *fq
	n := len(old)
	fetchInfo := old[n-1]
	old[n-1] = nil
	*fq = old[0 : n-1]
	return fetchInfo
}

func (fq *FetchQueue) Top() *FetchInfo {
	old := *fq
	if len(old) == 0 {
		return nil
	}
	return old[0]
}

func workerPanicHandler(p interface{}) {
	glog.Errorf("floor fetcher worker panicked: %v", p)
}

func NewPriceFloorFetcher(maxWorkers, maxCapacity, cacheSize int, httpClient *http.Client, metricEngine metrics.MetricsEngine) *PriceFloorFetcher {
	floorFetcher := PriceFloorFetcher{
		pool:            pond.New(maxWorkers, maxCapacity, pond.PanicHandler(workerPanicHandler)),
		fetchQueue:      make(FetchQueue, 0, 100),
		fetchInProgress: make(map[string]bool),
		configReceiver:  make(chan FetchInfo, maxCapacity),
		done:            make(chan struct{}),
		cache:           freecache.NewCache(cacheSize * 1024 * 1024),
		httpClient:      httpClient,
		time:            &timeutil.RealTime{},
		metricEngine:    metricEngine,
	}

	go floorFetcher.Fetcher()

	return &floorFetcher
}

func (f *PriceFloorFetcher) SetWithExpiry(key string, value json.RawMessage, cacheExpiry int) {
	f.cache.Set([]byte(key), value, cacheExpiry)
}

func (f *PriceFloorFetcher) Get(key string) (json.RawMessage, bool) {
	data, err := f.cache.Get([]byte(key))
	if err != nil {
		return nil, false
	}

	return data, true
}

func (f *PriceFloorFetcher) Fetch(config config.AccountPriceFloors) (*openrtb_ext.PriceFloorRules, string) {
	if !config.UseDynamicData || len(config.Fetcher.URL) == 0 || !validator.IsURL(config.Fetcher.URL) {
		return nil, openrtb_ext.FetchNone
	}

	// Check for floors JSON in cache
	result, found := f.Get(config.Fetcher.URL)
	if found {
		var fetchedFloorData openrtb_ext.PriceFloorRules
		err := json.Unmarshal(result, &fetchedFloorData)
		if err != nil || fetchedFloorData.Data == nil {
			return nil, openrtb_ext.FetchError
		}
		return &fetchedFloorData, openrtb_ext.FetchSuccess
	}

	//miss: push to channel to fetch and return empty response
	if config.Enabled && config.Fetcher.Enabled && config.Fetcher.Timeout > 0 {
		fetchInfo := FetchInfo{AccountFloorFetch: config.Fetcher, FetchTime: f.time.Now().Unix(), RefetchRequest: false}
		f.configReceiver <- fetchInfo
	}

	return nil, openrtb_ext.FetchInprogress
}

func (f *PriceFloorFetcher) worker(configs config.AccountFloorFetch) {
	floorData, fetchedMaxAge := f.fetchAndValidate(configs)
	if floorData != nil {
		// Update cache with new floor rules
		glog.Infof("Updating Value in cache for URL %s", configs.URL)
		cacheExpiry := configs.MaxAge
		if fetchedMaxAge != 0 && fetchedMaxAge > configs.Period && fetchedMaxAge < math.MaxInt32 {
			cacheExpiry = fetchedMaxAge
		}
		floorData, err := json.Marshal(floorData)
		if err != nil {
			glog.Errorf("Error while marshaling fetched floor data for url %s", configs.URL)
		} else {
			f.SetWithExpiry(configs.URL, floorData, cacheExpiry)
		}
	}

	// Send to refetch channel
	f.configReceiver <- FetchInfo{AccountFloorFetch: configs, FetchTime: f.time.Now().Add(time.Duration(configs.Period) * time.Second).Unix(), RefetchRequest: true}

}

// Stop terminates fetcher service can closes all the channels and destroys worker pool
func (f *PriceFloorFetcher) Stop() {
	close(f.done)
	f.pool.Stop()
	close(f.configReceiver)
}

func (f *PriceFloorFetcher) submit(fetchInfo *FetchInfo) {
	status := f.pool.TrySubmit(func() {
		f.worker(fetchInfo.AccountFloorFetch)
	})
	if !status {
		heap.Push(&f.fetchQueue, fetchInfo)
	}
}

func (f *PriceFloorFetcher) Fetcher() {
	//Create Ticker of 5 minutes
	ticker := time.NewTicker(time.Duration(refetchCheckInterval) * time.Second)

	for {
		select {
		case fetchInfo := <-f.configReceiver:
			if fetchInfo.RefetchRequest {
				heap.Push(&f.fetchQueue, &fetchInfo)
			} else {
				if _, ok := f.fetchInProgress[fetchInfo.URL]; !ok {
					f.fetchInProgress[fetchInfo.URL] = true
					f.submit(&fetchInfo)
				}
			}
		case <-ticker.C:
			currentTime := f.time.Now().Unix()
			for top := f.fetchQueue.Top(); top != nil && top.FetchTime <= currentTime; top = f.fetchQueue.Top() {
				nextFetch := heap.Pop(&f.fetchQueue)
				f.submit(nextFetch.(*FetchInfo))
			}
		case <-f.done:
			glog.Info("Price Floor fetcher terminated")
			return
		}
	}
}

func (f *PriceFloorFetcher) fetchAndValidate(config config.AccountFloorFetch) (*openrtb_ext.PriceFloorRules, int) {
	floorResp, maxAge, err := f.fetchFloorRulesFromURL(config)
	if err != nil {
		glog.Errorf("Error while fetching floor data from URL: %s, reason : %s", config.URL, err.Error())
		return nil, 0
	}

	if len(floorResp) > (config.MaxFileSizeKB * 1024) {
		glog.Errorf("Recieved invalid floor data from URL: %s, reason : floor file size is greater than MaxFileSize", config.URL)
		return nil, 0
	}

	var priceFloors openrtb_ext.PriceFloorRules
	if err = json.Unmarshal(floorResp, &priceFloors.Data); err != nil {
		glog.Errorf("Recieved invalid price floor json from URL: %s", config.URL)
		return nil, 0
	} else {
		err := validateRules(config, &priceFloors)
		if err != nil {
			glog.Errorf("Validation failed for floor JSON from URL: %s, reason: %s", config.URL, err.Error())
			return nil, 0
		}
	}

	return &priceFloors, maxAge
}

// fetchFloorRulesFromURL returns a price floor JSON and time for which this JSON is valid
// from provided URL with timeout constraints
func (f *PriceFloorFetcher) fetchFloorRulesFromURL(configs config.AccountFloorFetch) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(configs.Timeout)*time.Millisecond)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, configs.URL, nil)
	if err != nil {
		return nil, 0, errors.New("error while forming http fetch request : " + err.Error())
	}

	httpResp, err := f.httpClient.Do(httpReq)
	if err != nil {
		return nil, 0, errors.New("error while getting response from url : " + err.Error())
	}

	if httpResp.StatusCode != 200 {
		return nil, 0, errors.New("no response from server")
	}

	var maxAge int
	if maxAgeStr := httpResp.Header.Get("max-age"); maxAgeStr != "" {
		maxAge, err = strconv.Atoi(maxAgeStr)
		if err != nil {
			glog.Errorf("max-age in header is malformed for url %s", configs.URL)
		}
		if maxAge <= configs.Period {
			glog.Errorf("Invalid max-age = %s provided, value should be valid integer and should be within (%v, %v)", maxAgeStr, configs.Period, math.MaxInt32)
		}
	}

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, 0, errors.New("unable to read response")
	}
	defer httpResp.Body.Close()

	return respBody, maxAge, nil
}

func validateRules(configs config.AccountFloorFetch, priceFloors *openrtb_ext.PriceFloorRules) error {
	if priceFloors.Data == nil {
		return errors.New("empty data in floor JSON")
	}

	if len(priceFloors.Data.ModelGroups) == 0 {
		return errors.New("no model groups found in price floor data")
	}

	if priceFloors.Data.SkipRate < 0 || priceFloors.Data.SkipRate > 100 {
		return errors.New("skip rate should be greater than or equal to 0 and less than 100")
	}

	for _, modelGroup := range priceFloors.Data.ModelGroups {
		if len(modelGroup.Values) == 0 || len(modelGroup.Values) > configs.MaxRules {
			return errors.New("invalid number of floor rules, floor rules should be greater than zero and less than MaxRules specified in account config")
		}

		if modelGroup.ModelWeight != nil && (*modelGroup.ModelWeight < 1 || *modelGroup.ModelWeight > 100) {
			return errors.New("modelGroup[].modelWeight should be greater than or equal to 1 and less than 100")
		}

		if modelGroup.SkipRate < 0 || modelGroup.SkipRate > 100 {
			return errors.New("model group skip rate should be greater than or equal to 0 and less than 100")
		}

		if modelGroup.Default < 0 {
			return errors.New("modelGroup.Default should be greater than 0")
		}
	}

	return nil
}
