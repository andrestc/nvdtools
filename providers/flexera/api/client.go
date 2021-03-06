// Copyright (c) Facebook, Inc. and its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/facebookincubator/nvdtools/providers/flexera/rate"
	"github.com/facebookincubator/nvdtools/providers/flexera/schema"
	"github.com/pkg/errors"
)

// Client stores information needed to access Flexera API
// API key will be sent in the Authorization field
// rate limiter is used to enforce their api limits (so we don't go over them)
type Client struct {
	apiKey  string
	baseURL string
	limiter rate.Limiter
}

const (
	pageSize           = 100
	userAgent          = "fb-flexera"
	advisoriesEndpoint = "/api/advisories"
	numFetchers        = 4
	requestsPerMinute  = 240
	numRequestRetries  = 3
	retryDelay         = 1 * time.Second
)

// NewClient creates a new Client object with given properties
func NewClient(baseURL, apiKey string) Client {
	return Client{
		apiKey:  apiKey,
		baseURL: baseURL,
		limiter: rate.BurstyLimiter(time.Minute, requestsPerMinute),
	}
}

// FetchAll will fetch all advisories since given time
// we first fetch all pages and just collect all identifiers found on them and
// push them into the `identifiers` channel. Then we start fetchers which take
// those identifiers and fetch the real advisories
func (c Client) FetchAll(from, to int64) (<-chan *schema.FlexeraAdvisory, error) {
	totalAdvisories, err := c.getNumberOfAdvisories(from, to)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get total number of advisories")
	}

	numPages := (totalAdvisories-1)/pageSize + 1
	log.Printf("starting sync for %d advisories over %d pages\n", totalAdvisories, numPages)

	identifiers := make(chan string, totalAdvisories)
	advisories := make(chan *schema.FlexeraAdvisory, totalAdvisories)

	wgPages := sync.WaitGroup{}
	for page := 0; page < numPages; page++ {
		wgPages.Add(1)
		go func(p int) {
			defer wgPages.Done()
			list, err := c.fetchAdvisoryList(from, to, p)
			if err == nil {
				for _, element := range list.Results {
					identifiers <- element.AdvisoryIdentifier
				}
			} else {
				log.Println(errors.Wrapf(err, "failed to fetch page %d advisory list", p))
			}
		}(page + 1)
	}

	go func() {
		wgPages.Wait()
		close(identifiers)
	}()

	wgFetcher := sync.WaitGroup{}
	for i := 0; i < numFetchers; i++ {
		wgFetcher.Add(1)
		go func() {
			defer wgFetcher.Done()
			for identifier := range identifiers {
				advisory, err := c.Fetch(identifier)
				if err == nil {
					advisories <- advisory
				} else {
					log.Println(errors.Wrapf(err, "failed to fetch advisory %s", identifier))
				}
			}
		}()
	}

	go func() {
		wgFetcher.Wait()
		close(advisories)
	}()

	return advisories, nil
}

// Fetch will return a channel with only one advisory in it
func (c Client) Fetch(identifier string) (*schema.FlexeraAdvisory, error) {
	var advisory schema.FlexeraAdvisory
	endpoint := fmt.Sprintf("%s/%s", advisoriesEndpoint, identifier)
	if err := c.query(endpoint, map[string]string{}, &advisory); err != nil {
		return nil, errors.Wrapf(err, "failed to query advisory details endpoint %s", endpoint)
	}
	return &advisory, nil
}

func (c Client) fetchAdvisoryList(from, to int64, page int) (*schema.FlexeraAdvisoryListResult, error) {
	var list schema.FlexeraAdvisoryListResult
	params := map[string]string{
		"released__gte": strconv.FormatInt(from, 10),
		"released__lt":  strconv.FormatInt(to, 10),
		"page":          strconv.Itoa(page),
		"page_size":     strconv.Itoa(pageSize),
	}
	if err := c.query(advisoriesEndpoint, params, &list); err != nil {
		return nil, errors.Wrapf(err, "failed to fetch page %d", page)
	}
	return &list, nil
}

func (c Client) getNumberOfAdvisories(from, to int64) (int, error) {
	var list schema.FlexeraAdvisoryListResult
	params := map[string]string{
		"released__gte": strconv.FormatInt(from, 10),
		"released__lt":  strconv.FormatInt(to, 10),
		"page_size":     "1",
	}
	if err := c.query(advisoriesEndpoint, params, &list); err != nil {
		return 0, errors.Wrap(err, "failed to fetch first page")
	}
	return list.Count, nil
}

func (c Client) query(endpoint string, params map[string]string, v interface{}) error {
	// setup new parameters
	u, err := url.Parse(fmt.Sprintf("%s%s", c.baseURL, endpoint))
	if err != nil {
		return errors.Wrap(err, "failed to parse client URL")
	}
	query := u.Query()
	for key, value := range params {
		query.Set(key, value)
	}
	u.RawQuery = query.Encode()

	var resp *http.Response
	for i := 0; i < numRequestRetries; i++ {
		c.limiter.Allow() // block until we can make another request
		resp, err = queryURL(u.String(), http.Header{
			"Authorization": {c.apiKey},
			"User-Agent":    {userAgent},
		})
		if err == nil {
			break
		}

		if he, ok := err.(httpError); !ok || !he.isRateLimit() {
			return err
		}
		// it is rate limit, just retry after 1 second
		time.Sleep(retryDelay)
	}
	if resp == nil {
		return err
	}

	defer resp.Body.Close()

	// decode into json
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return errors.Wrap(err, "failed to decode response")
	}

	return nil
}
