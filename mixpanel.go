package mixpanel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"
)

var IgnoreTime *time.Time = &time.Time{}

type MixpanelError struct {
	URL string
	Err error
}

func (err *MixpanelError) Cause() error {
	return err.Err
}

func (err *MixpanelError) Error() string {
	return "mixpanel: " + err.Err.Error()
}

func (err *MixpanelError) Unwrap() error {
	if err == nil {
		return nil
	}

	return err.Err
}

type ErrTrackFailed struct {
	Message  string
	Body     []byte
	HTTPCode int
}

func (err *ErrTrackFailed) Error() string {
	return fmt.Sprintf("mixpanel did not return 1 when tracking: %s", err.Message)
}

// The Mixapanel struct store the mixpanel endpoint and the project token
type Mixpanel interface {
	// Create a mixpanel event using the track api
	Track(ctx context.Context, distinctId, eventName string, e *Event) error

	// Create a mixpanel event using the import api
	Import(ctx context.Context, distinctId, eventName string, e *Event) error

	ImportBatch(ctx context.Context, events []*TrackEvent) error

	// Set properties for a mixpanel user.
	// Deprecated: Use UpdateUser instead
	Update(ctx context.Context, distinctId string, u *Update) error

	// Set properties for a mixpanel user.
	UpdateUser(ctx context.Context, distinctId string, u *Update) error

	// Set properties for a mixpanel group.
	UpdateGroup(ctx context.Context, groupKey, groupId string, u *Update) error

	// Create an alias for an existing distinct id
	Alias(ctx context.Context, distinctId, newId string) error
}

// The Mixapanel struct store the mixpanel endpoint and the project token
type mixpanel struct {
	Client *http.Client
	Token  string
	Secret string
	ApiURL string
}

// A mixpanel event
type Event struct {
	// IP-address of the user. Leave empty to use autodetect, or set to "0" to
	// not specify an ip-address.
	IP string

	// Timestamp. Set to nil to use the current time.
	Timestamp *time.Time

	// Custom properties. At least one must be specified.
	Properties map[string]interface{}
}

type TrackEvent struct {
	DistinctID string
	EventName  string
	Event      *Event
}

// An update of a user in mixpanel
type Update struct {
	// IP-address of the user. Leave empty to use autodetect, or set to "0" to
	// not specify an ip-address at all.
	IP string

	// Timestamp. Set to nil to use the current time, or IgnoreTime to not use a
	// timestamp.
	Timestamp *time.Time

	// Update operation such as "$set", "$update" etc.
	Operation string

	// Custom properties. At least one must be specified.
	Properties map[string]interface{}
}

// Alias create an alias for an existing distinct id
func (m *mixpanel) Alias(ctx context.Context, distinctId, newId string) error {
	props := map[string]interface{}{
		"token":       m.Token,
		"distinct_id": distinctId,
		"alias":       newId,
	}

	params := map[string]interface{}{
		"event":      "$create_alias",
		"properties": props,
	}

	return m.send(ctx, "track", params, false)
}

func (m *mixpanel) eventToParams(distinctID, eventName string, e *Event) map[string]interface{} {
	props := map[string]interface{}{
		"token":       m.Token,
		"distinct_id": distinctID,
	}
	if e.IP != "" {
		props["ip"] = e.IP
	}
	if e.Timestamp != nil {
		props["time"] = e.Timestamp.Unix()
	}

	for key, value := range e.Properties {
		props[key] = value
	}

	params := map[string]interface{}{
		"event":      eventName,
		"properties": props,
	}

	return params
}

// Track create an event for an existing distinct id
func (m *mixpanel) Track(ctx context.Context, distinctID, eventName string, e *Event) error {
	autoGeolocate := e.IP == ""
	return m.send(ctx, "track", m.eventToParams(distinctID, eventName, e), autoGeolocate)
}

// Import create an event for an existing distinct id
// See https://developer.mixpanel.com/docs/importing-old-events
func (m *mixpanel) Import(ctx context.Context, distinctID, eventName string, e *Event) error {
	autoGeolocate := e.IP == ""
	return m.sendImport(ctx, m.eventToParams(distinctID, eventName, e), autoGeolocate)
}

// Import batch takes a batch of events and imports them all.
func (m *mixpanel) ImportBatch(ctx context.Context, events []*TrackEvent) error {
	if len(events) == 0 {
		return nil
	}

	params := []map[string]interface{}{}

	for _, event := range events {
		params = append(params, m.eventToParams(event.DistinctID, event.EventName, event.Event))
	}

	return m.sendImport(ctx, params, false)
}

// Update updates a user in mixpanel. See
// https://mixpanel.com/help/reference/http#people-analytics-updates
// Deprecated: Use UpdateUser instead
func (m *mixpanel) Update(ctx context.Context, distinctId string, u *Update) error {
	return m.UpdateUser(ctx, distinctId, u)
}

// UpdateUser: Updates a user in mixpanel. See
// https://mixpanel.com/help/reference/http#people-analytics-updates
func (m *mixpanel) UpdateUser(ctx context.Context, distinctId string, u *Update) error {
	params := map[string]interface{}{
		"$token":       m.Token,
		"$distinct_id": distinctId,
	}

	if u.IP != "" {
		params["$ip"] = u.IP
	}
	if u.Timestamp == IgnoreTime {
		params["$ignore_time"] = true
	} else if u.Timestamp != nil {
		params["$time"] = u.Timestamp.Unix()
	}

	params[u.Operation] = u.Properties

	autoGeolocate := u.IP == ""

	return m.send(ctx, "engage", params, autoGeolocate)
}

// UpdateGroup: Updates a group in mixpanel. See
// https://api.mixpanel.com/groups#group-set
func (m *mixpanel) UpdateGroup(ctx context.Context, groupKey, groupId string, u *Update) error {
	params := map[string]interface{}{
		"$token":     m.Token,
		"$group_id":  groupId,
		"$group_key": groupKey,
	}

	params[u.Operation] = u.Properties

	return m.send(ctx, "groups", params, false)
}

func (m *mixpanel) to64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

func (m *mixpanel) sendImport(ctx context.Context, params interface{}, autoGeolocate bool) error {
	data, err := json.Marshal(params)

	if err != nil {
		return err
	}

	url := m.ApiURL + "/import?strict=1"

	wrapErr := func(err error) error {
		return &MixpanelError{URL: url, Err: err}
	}

	request, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader(string(data)))
	if err != nil {
		return wrapErr(err)
	}
	if m.Secret != "" {
		request.SetBasicAuth(m.Secret, "")
	}
	request.Header.Set("Content-Type", "application/json")
	resp, err := m.Client.Do(request)
	if err != nil {
		return wrapErr(err)
	}

	defer resp.Body.Close()

	body, bodyErr := ioutil.ReadAll(resp.Body)

	if bodyErr != nil {
		return wrapErr(bodyErr)
	}

	type verboseResponse struct {
		Error  string `json:"error"`
		Status string `json:"status"`
	}

	var jsonBody verboseResponse
	err = json.Unmarshal(body, &jsonBody)
	if err != nil {
		return wrapErr(err)
	}

	// TODO(joey): If some records in the batch failed, return them so they can be retried.
	if jsonBody.Status != "OK" {
		errMsg := fmt.Sprintf("error=%s; status=%s; httpCode=%d, body=%s", jsonBody.Error, jsonBody.Status, resp.StatusCode, string(body))
		return wrapErr(&ErrTrackFailed{Message: errMsg, HTTPCode: resp.StatusCode, Body: body})
	}

	return nil
}

func (m *mixpanel) send(ctx context.Context, eventType string, params interface{}, autoGeolocate bool) error {
	data, err := json.Marshal(params)

	if err != nil {
		return err
	}

	url := m.ApiURL + "/" + eventType + "?verbose=1"

	wrapErr := func(err error) error {
		return &MixpanelError{URL: url, Err: err}
	}

	request, err := http.NewRequestWithContext(ctx, "POST", url, strings.NewReader("data="+m.to64(data)))
	if err != nil {
		return wrapErr(err)
	}
	if m.Secret != "" {
		request.SetBasicAuth(m.Secret, "")
	}
	resp, err := m.Client.Do(request)
	if err != nil {
		return wrapErr(err)
	}

	defer resp.Body.Close()

	body, bodyErr := ioutil.ReadAll(resp.Body)

	if bodyErr != nil {
		return wrapErr(bodyErr)
	}

	type verboseResponse struct {
		Error  string `json:"error"`
		Status int    `json:"status"`
	}

	var jsonBody verboseResponse
	json.Unmarshal(body, &jsonBody)

	if jsonBody.Status != 1 {
		errMsg := fmt.Sprintf("error=%s; status=%d; httpCode=%d", jsonBody.Error, jsonBody.Status, resp.StatusCode)
		return wrapErr(&ErrTrackFailed{Message: errMsg, HTTPCode: resp.StatusCode, Body: body})
	}

	return nil
}

// New returns the client instance. If apiURL is blank, the default will be used
// ("https://api.mixpanel.com").
func New(token, apiURL string) Mixpanel {
	return NewFromClient(http.DefaultClient, token, apiURL)
}

// NewWithSecret returns the client instance using a secret.If apiURL is blank,
// the default will be used ("https://api.mixpanel.com").
func NewWithSecret(token, secret, apiURL string) Mixpanel {
	return NewFromClientWithSecret(http.DefaultClient, token, secret, apiURL)
}

// NewFromClient creates a client instance using the specified client instance. This is useful
// when using a proxy.
func NewFromClient(c *http.Client, token, apiURL string) Mixpanel {
	return NewFromClientWithSecret(c, token, "", apiURL)
}

// NewFromClientWithSecret creates a client instance using the specified client instance and secret.
func NewFromClientWithSecret(c *http.Client, token, secret, apiURL string) Mixpanel {
	if apiURL == "" {
		apiURL = "https://api.mixpanel.com"
	}

	return &mixpanel{
		Client: c,
		Token:  token,
		Secret: secret,
		ApiURL: apiURL,
	}
}
