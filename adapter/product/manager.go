// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package product

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/apigee/istio-mixer-adapter/adapter/auth"
	"github.com/apigee/istio-mixer-adapter/adapter/util"
	"istio.io/istio/mixer/pkg/adapter"
)

const productsURL = "/products"

/*
Usage:
	pp := createManager()
	pp.start()
	products := pp.Products()
	...
	pp.close() // when done
*/

func createManager(options Options, log adapter.Logger) *Manager {
	return &Manager{
		baseURL:     options.BaseURL,
		log:         log,
		closedChan:  make(chan bool),
		returnChan:  make(chan map[string]*APIProduct),
		closed:      util.NewAtomicBool(false),
		refreshRate: options.RefreshRate,
		client:      options.Client,
		key:         options.Key,
		secret:      options.Secret,
	}
}

// A Manager wraps all things related to a set of API products.
type Manager struct {
	baseURL          *url.URL
	log              adapter.Logger
	closed           *util.AtomicBool
	closedChan       chan bool
	returnChan       chan map[string]*APIProduct
	refreshRate      time.Duration
	refreshTimerChan <-chan time.Time
	client           *http.Client
	key              string
	secret           string
	productsMux      productsMux
	cancelPolling    context.CancelFunc
}

func (p *Manager) start(env adapter.Env) {
	p.log.Infof("starting product manager")
	p.productsMux = productsMux{
		setChan:   make(chan ProductsMap),
		getChan:   make(chan ProductsMap),
		closeChan: make(chan struct{}),
		closed:    util.NewAtomicBool(false),
	}
	env.ScheduleDaemon(func() {
		p.productsMux.mux()
	})

	poller := util.Looper{
		Env:     env,
		Backoff: util.NewExponentialBackoff(200*time.Millisecond, p.refreshRate, 2, true),
	}
	apiURL := *p.baseURL
	apiURL.Path = path.Join(apiURL.Path, productsURL)
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelPolling = cancel
	poller.Start(ctx, p.pollingClosure(apiURL), p.refreshRate, func(err error) error {
		p.log.Errorf("Error retrieving products: %v", err)
		return nil
	})

	p.log.Infof("started product manager")
}

// Products atomically gets a mapping of name => APIProduct.
func (p *Manager) Products() ProductsMap {
	if p.closed.IsTrue() {
		return nil
	}
	return p.productsMux.Get()
}

// Close shuts down the manager.
func (p *Manager) Close() {
	if p == nil || p.closed.SetTrue() {
		return
	}
	p.log.Infof("closing product manager")
	p.cancelPolling()
	p.productsMux.Close()
	p.log.Infof("closed product manager")
}

func (p *Manager) pollingClosure(apiURL url.URL) func(ctx context.Context) error {
	return func(ctx context.Context) error {

		req, err := http.NewRequest(http.MethodGet, apiURL.String(), nil)
		if err != nil {
			return err
		}
		req = req.WithContext(ctx) // make cancelable from poller

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.SetBasicAuth(p.key, p.secret)

		p.log.Debugf("retrieving products from: %s", apiURL.String())

		resp, err := p.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			p.log.Errorf("Unable to read server response: %v", err)
			return err
		}

		if resp.StatusCode != 200 {
			return p.log.Errorf("products request failed (%d): %s", resp.StatusCode, string(body))
		}

		var res APIResponse
		err = json.Unmarshal(body, &res)
		if err != nil {
			p.log.Errorf("unable to unmarshal JSON response '%s': %v", string(body), err)
			return err
		}

		pm := p.getProductsMap(ctx, res)
		p.productsMux.Set(pm)

		p.log.Debugf("retrieved %d products, kept %d", len(res.APIProducts), len(pm))

		return nil
	}
}

func (p *Manager) getProductsMap(ctx context.Context, res APIResponse) ProductsMap {
	pm := ProductsMap{}
	for _, v := range res.APIProducts {
		product := v
		// only save products that actually map to something
		for _, attr := range product.Attributes {
			if ctx.Err() != nil {
				p.log.Debugf("product polling canceled, exiting")
				return nil
			}
			if attr.Name == ServicesAttr {
				var err error
				targets := strings.Split(attr.Value, ",")
				for _, t := range targets {
					product.Targets = append(product.Targets, strings.TrimSpace(t))
				}

				// server returns empty scopes as array with a single empty string, remove for consistency
				if len(product.Scopes) == 1 && product.Scopes[0] == "" {
					product.Scopes = []string{}
				}

				// parse limit from server
				if product.QuotaLimit != "" && product.QuotaInterval != "null" {
					product.QuotaLimitInt, err = strconv.ParseInt(product.QuotaLimit, 10, 64)
					if err != nil {
						p.log.Errorf("unable to parse quota limit: %#v", product)
					}
				}

				// parse interval from server
				if product.QuotaInterval != "" && product.QuotaInterval != "null" {
					product.QuotaIntervalInt, err = strconv.ParseInt(product.QuotaInterval, 10, 64)
					if err != nil {
						p.log.Errorf("unable to parse quota interval: %#v", product)
					}
				}

				// normalize null from server to empty
				if product.QuotaTimeUnit == "null" {
					product.QuotaTimeUnit = ""
				}

				p.resolveResourceMatchers(&product)

				pm[product.Name] = &product
				break
			}
		}
	}
	return pm
}

// generate matchers for resources (path)
func (p *Manager) resolveResourceMatchers(product *APIProduct) {
	for _, resource := range product.Resources {
		reg, err := makeResourceRegex(resource)
		if err != nil {
			p.log.Errorf("unable to create resource matcher: %#v", product)
			continue
		}
		product.resourceRegexps = append(product.resourceRegexps, reg)
	}
}

// Resolve determines the valid products for a given API.
func (p *Manager) Resolve(ac *auth.Context, api, path string) []*APIProduct {
	validProducts, failHints := resolve(ac, p.Products(), api, path)
	var selected []string
	for _, p := range validProducts {
		selected = append(selected, p.Name)
	}
	ac.Log().Debugf(`
Resolve api: %s, path: %s, scopes: %v
Selected: %v
Eliminated: %v`, api, path, ac.Scopes, selected, failHints)
	return validProducts
}

func resolve(ac *auth.Context, pMap map[string]*APIProduct, api, path string) (
	result []*APIProduct, failHints []string) {

	for _, name := range ac.APIProducts {
		apiProduct, ok := pMap[name]
		if !ok {
			failHints = append(failHints, fmt.Sprintf("%s doesn't exist", name))
			continue
		}
		// if APIKey, scopes don't matter
		if ac.APIKey == "" && !apiProduct.isValidScopes(ac.Scopes) {
			failHints = append(failHints, fmt.Sprintf("%s doesn't match scopes: %s", name, ac.Scopes))
			continue
		}
		if !apiProduct.isValidPath(path) {
			failHints = append(failHints, fmt.Sprintf("%s doesn't match path: %s", name, path))
			continue
		}
		if !apiProduct.isValidTarget(api) {
			failHints = append(failHints, fmt.Sprintf("%s doesn't match target: %s", name, api))
			continue
		}
		result = append(result, apiProduct)
	}
	return result, failHints
}

// true if valid target for API Product
func (p *APIProduct) isValidTarget(api string) bool {
	for _, target := range p.Targets {
		if target == api {
			return true
		}
	}
	return false
}

// true if valid path for API Product
func (p *APIProduct) isValidPath(requestPath string) bool {
	for _, reg := range p.resourceRegexps {
		if reg.MatchString(requestPath) {
			return true
		}
	}
	return false
}

// true if any intersect of scopes (or no product scopes)
func (p *APIProduct) isValidScopes(scopes []string) bool {
	if len(p.Scopes) == 0 {
		return true
	}
	for _, ds := range p.Scopes {
		for _, s := range scopes {
			if ds == s {
				return true
			}
		}
	}
	return false
}

// GetServicesAttribute returns a pointer the services attribute or nil
func (p *APIProduct) GetServicesAttribute() *Attribute {
	for _, attr := range p.Attributes {
		if attr.Name == ServicesAttr {
			return &attr
		}
	}
	return nil
}

// GetBoundServices returns an array of service names bound to this product
func (p *APIProduct) GetBoundServices() []string {
	attr := p.GetServicesAttribute()
	if attr != nil {
		return strings.Split(attr.Value, ",")
	}
	return nil
}

// - A single slash by itself matches any path
// - * is valid anywhere and matches within a segment (between slashes)
// - ** is valid only at the end and matches anything to EOL
func makeResourceRegex(resource string) (*regexp.Regexp, error) {

	if resource == "/" {
		return regexp.Compile(".*")
	}

	// only allow ** as suffix
	doubleStarIndex := strings.Index(resource, "**")
	if doubleStarIndex >= 0 && doubleStarIndex != len(resource)-2 {
		return nil, fmt.Errorf("bad resource specification")
	}

	// remove ** suffix if exists
	pattern := resource
	if doubleStarIndex >= 0 {
		pattern = pattern[:len(pattern)-2]
	}

	// let * = any non-slash
	pattern = strings.Replace(pattern, "*", "[^/]*", -1)

	// if ** suffix, allow anything at end
	if doubleStarIndex >= 0 {
		pattern = pattern + ".*"
	}

	return regexp.Compile("^" + pattern + "$")
}

// ProductsMap is a map of API Product name to API Product
type ProductsMap map[string]*APIProduct

type productsMux struct {
	setChan   chan ProductsMap
	getChan   chan ProductsMap
	closeChan chan struct{}
	closed    *util.AtomicBool
}

func (h productsMux) Get() ProductsMap {
	return <-h.getChan
}

func (h productsMux) Set(s ProductsMap) {
	if h.closed.IsFalse() {
		h.setChan <- s
	}
}

func (h productsMux) Close() {
	if !h.closed.SetTrue() {
		close(h.closeChan)
	}
}

func (h productsMux) mux() {
	var productsMap ProductsMap
	for {
		if productsMap == nil {
			select {
			case <-h.closeChan:
				close(h.setChan)
				close(h.getChan)
				return
			case productsMap = <-h.setChan:
				continue
			}
		}
		select {
		case productsMap = <-h.setChan:
		case h.getChan <- productsMap:
		case <-h.closeChan:
			close(h.setChan)
			close(h.getChan)
			return
		}
	}
}
