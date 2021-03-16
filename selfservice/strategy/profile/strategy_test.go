package profile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ory/kratos-client-go"

	"github.com/ory/kratos/corpx"

	"github.com/julienschmidt/httprouter"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/identity"
	"github.com/ory/kratos/internal"
	"github.com/ory/kratos/internal/testhelpers"
	"github.com/ory/kratos/selfservice/flow/settings"
	"github.com/ory/kratos/x"
	"github.com/ory/x/assertx"
	"github.com/ory/x/httpx"
	"github.com/ory/x/ioutilx"
	"github.com/ory/x/pointerx"
	"github.com/ory/x/sqlxx"
)

func init() {
	corpx.RegisterFakes()
}

func newIdentityWithPassword(email string) *identity.Identity {
	return &identity.Identity{
		ID: x.NewUUID(),
		Credentials: map[identity.CredentialsType]identity.Credentials{
			"password": {Type: "password", Identifiers: []string{email}, Config: sqlxx.JSONRawMessage(`{"hashed_password":"foo"}`)},
		},
		Traits:              identity.Traits(`{"email":"` + email + `","stringy":"foobar","booly":false,"numby":2.5,"should_long_string":"asdfasdfasdfasdfasfdasdfasdfasdf","should_big_number":2048}`),
		SchemaID:            config.DefaultIdentityTraitsSchemaID,
		VerifiableAddresses: []identity.VerifiableAddress{{Value: email, Via: identity.VerifiableAddressTypeEmail}},
		// TO ADD - RECOVERY EMAIL,
	}
}

func TestStrategyTraits(t *testing.T) {
	conf, reg := internal.NewFastRegistryWithMocks(t)
	conf.MustSet(config.ViperKeyDefaultIdentitySchemaURL, "file://./stub/identity.schema.json")
	conf.MustSet(config.ViperKeySelfServiceBrowserDefaultReturnTo, "https://www.ory.sh/")
	conf.MustSet(config.ViperKeySelfServiceSettingsPrivilegedAuthenticationAfter, "1ns")
	testhelpers.StrategyEnable(t, conf, identity.CredentialsTypePassword.String(), true)
	testhelpers.StrategyEnable(t, conf, settings.StrategyProfile, true)

	ui := testhelpers.NewSettingsUIEchoServer(t, reg)
	_ = testhelpers.NewErrorTestServer(t, reg)

	publicTS, adminTS := testhelpers.NewKratosServer(t, reg)

	browserIdentity1 := newIdentityWithPassword("john-browser@doe.com")
	apiIdentity1 := newIdentityWithPassword("john-api@doe.com")
	browserIdentity2 := &identity.Identity{ID: x.NewUUID(), Traits: identity.Traits(`{}`)}
	apiIdentity2 := &identity.Identity{ID: x.NewUUID(), Traits: identity.Traits(`{}`)}

	browserUser1 := testhelpers.NewHTTPClientWithIdentitySessionCookie(t, reg, browserIdentity1)
	browserUser2 := testhelpers.NewHTTPClientWithIdentitySessionCookie(t, reg, browserIdentity2)
	apiUser1 := testhelpers.NewHTTPClientWithIdentitySessionToken(t, reg, apiIdentity1)
	apiUser2 := testhelpers.NewHTTPClientWithIdentitySessionToken(t, reg, apiIdentity2)

	adminClient := testhelpers.NewSDKClient(adminTS)

	t.Run("description=not authorized to call endpoints without a session", func(t *testing.T) {
		t.Run("type=browser", func(t *testing.T) {
			res, err := http.DefaultClient.Do(httpx.MustNewRequest("POST", publicTS.URL+settings.RouteSubmitFlow, strings.NewReader(url.Values{"foo": {"bar"}}.Encode()), "application/x-www-form-urlencoded"))
			require.NoError(t, err)
			defer res.Body.Close()
			assert.EqualValues(t, http.StatusUnauthorized, res.StatusCode, "%+v", res.Request)
			assert.Contains(t, res.Request.URL.String(), conf.Source().String(config.ViperKeySelfServiceLoginUI))
		})

		t.Run("type=api", func(t *testing.T) {
			res, err := http.DefaultClient.Do(httpx.MustNewRequest("POST", publicTS.URL+settings.RouteSubmitFlow, strings.NewReader(`{"foo":"bar"}`), "application/json"))
			require.NoError(t, err)
			defer res.Body.Close()
			assert.EqualValues(t, http.StatusUnauthorized, res.StatusCode)
		})
	})

	t.Run("description=should fail to post data if CSRF is invalid/type=browser", func(t *testing.T) {
		f := testhelpers.InitializeSettingsFlowViaBrowser(t, browserUser1, publicTS)

		actual, res := testhelpers.SettingsMakeRequest(t, false, f, browserUser1,
			url.Values{"profile.traits.booly": {"true"}, "csrf_token": {"invalid"}, "method":{"profile"}}.Encode())
		assert.EqualValues(t, http.StatusOK, res.StatusCode, "should return a 400 error because CSRF token is not set\n\t%s", actual)
		assertx.EqualAsJSON(t, x.ErrInvalidCSRFToken, json.RawMessage(gjson.Get(actual, "0").Raw), actual)
	})

	t.Run("description=should not fail if CSRF token is invalid/type=api", func(t *testing.T) {
		f := testhelpers.InitializeSettingsFlowViaAPI(t, apiUser1, publicTS)

		actual, res := testhelpers.SettingsMakeRequest(t, true, f, apiUser1, `{"profile.traits.booly":true,"method":"profile","csrf_token":"invalid"}`)
		assert.Len(t, res.Cookies(), 0)
		assert.EqualValues(t, http.StatusBadRequest, res.StatusCode)
		assert.EqualValues(t, "api", gjson.Get(actual, "type").String())
	})

	t.Run("case=should fail with correct CSRF error cause/type=api", func(t *testing.T) {
		for k, tc := range []struct {
			mod func(http.Header)
			exp string
		}{
			{
				mod: func(h http.Header) {
					h.Add("Cookie", "name=bar")
				},
				exp: "The HTTP Request Header included the \\\"Cookie\\\" key",
			},
			{
				mod: func(h http.Header) {
					h.Add("Origin", "www.bar.com")
				},
				exp: "The HTTP Request Header included the \\\"Origin\\\" key",
			},
		} {
			t.Run(fmt.Sprintf("case=%d", k), func(t *testing.T) {
				f := testhelpers.InitializeSettingsFlowViaAPI(t, apiUser1, publicTS)

				req := testhelpers.NewRequest(t, true, "POST", f.Ui.Action, bytes.NewBufferString(`{"profile.traits.booly":true,"method":"profile","csrf_token":"invalid"}`))
				tc.mod(req.Header)

				res, err := apiUser1.Do(req)
				require.NoError(t, err)
				defer res.Body.Close()

				actual := string(ioutilx.MustReadAll(res.Body))
				assert.EqualValues(t, http.StatusBadRequest, res.StatusCode)
				assert.Contains(t, actual, tc.exp, "%s", actual)
			})
		}
	})

	t.Run("description=hydrate the proper fields", func(t *testing.T) {
		var run = func(t *testing.T, id *identity.Identity, payload *kratos.SettingsFlow, route string) {
			assert.NotEmpty(t, payload.Identity)
			assert.Equal(t, id.ID.String(), string(payload.Identity.Id))
			assert.JSONEq(t, string(id.Traits), x.MustEncodeJSON(t, payload.Identity.Traits))
			assert.Equal(t, id.SchemaID, payload.Identity.SchemaId)
			assert.Equal(t, publicTS.URL+route, payload.RequestUrl)

			assertx.EqualAsJSON(t, &kratos.SettingsFlowMethodConfig{
				Action: publicTS.URL + settings.RouteSubmitFlow + "?flow=" + string(payload.Id),
				Method: "POST",
				Nodes: []kratos.UiNode{
					*testhelpers.NewFakeCSRFNode(),
					{
						Type:  "input",
						Group: "default",
						Attributes: kratos.UiNodeInputAttributesAsUiNodeAttributes(&kratos.UiNodeInputAttributes{
							Name: "profile.traits.email",
							Type: "text",
							Value: &kratos.UiNodeInputAttributesValue{
								String: pointerx.String(gjson.GetBytes(id.Traits, "email").String()),
							},
						}),
					},
					{
						Type:  "input",
						Group: "default",
						Attributes: kratos.UiNodeInputAttributesAsUiNodeAttributes(&kratos.UiNodeInputAttributes{
							Name: "profile.traits.stringy",
							Type: "text",
							Value: &kratos.UiNodeInputAttributesValue{
								String: pointerx.String("foobar"),
							},
						}),
					},
					{
						Type:  "input",
						Group: "default",
						Attributes: kratos.UiNodeInputAttributesAsUiNodeAttributes(&kratos.UiNodeInputAttributes{
							Name: "profile.traits.numby",
							Type: "number",
							Value: &kratos.UiNodeInputAttributesValue{
								Float32: pointerx.Float32(2.5),
							},
						}),
					},
					{
						Type:  "input",
						Group: "default",
						Attributes: kratos.UiNodeInputAttributesAsUiNodeAttributes(&kratos.UiNodeInputAttributes{
							Name: "profile.traits.booly",
							Type: "checkbox",
							Value: &kratos.UiNodeInputAttributesValue{
								Bool: pointerx.Bool(false),
							},
						}),
					},
					{
						Type:  "input",
						Group: "default",
						Attributes: kratos.UiNodeInputAttributesAsUiNodeAttributes(&kratos.UiNodeInputAttributes{
							Name: "profile.traits.should_big_number",
							Type: "number",
							Value: &kratos.UiNodeInputAttributesValue{
								Float32: pointerx.Float32(2048),
							},
						}),
					},
					{
						Type:  "input",
						Group: "default",
						Attributes: kratos.UiNodeInputAttributesAsUiNodeAttributes(&kratos.UiNodeInputAttributes{
							Name: "profile.traits.should_long_string",
							Type: "text",
							Value: &kratos.UiNodeInputAttributesValue{
								String: pointerx.String("asdfasdfasdfasdfasfdasdfasdfasdf"),
							},
						}),
					},
				},
			}, payload)
		}

		t.Run("type=api", func(t *testing.T) {
			pr, _, err := testhelpers.NewSDKCustomClient(publicTS, apiUser1).PublicApi.InitializeSelfServiceSettingsViaAPIFlow(context.Background()).Execute()
			require.NoError(t, err)
			run(t, apiIdentity1, pr, settings.RouteInitAPIFlow)
		})

		t.Run("type=browser", func(t *testing.T) {
			res, err := browserUser1.Get(publicTS.URL + settings.RouteInitBrowserFlow)
			require.NoError(t, err)
			assert.Contains(t, res.Request.URL.String(), ui.URL+"/settings?flow")

			rid := res.Request.URL.Query().Get("flow")
			require.NotEmpty(t, rid)

			pr, res, err := testhelpers.NewSDKCustomClient(publicTS, browserUser1).PublicApi.GetSelfServiceSettingsFlow(context.Background()).Id(res.Request.URL.Query().Get("flow")).Execute()
			require.NoError(t, err, "%s", rid)

			run(t, browserIdentity1, pr, settings.RouteInitBrowserFlow)
		})
	})

	var expectValidationError = func(t *testing.T, isAPI bool, hc *http.Client, values func(url.Values)) string {
		return testhelpers.SubmitSettingsForm(t, isAPI, hc, publicTS, values,
			testhelpers.ExpectStatusCode(isAPI, http.StatusBadRequest, http.StatusOK),
			testhelpers.ExpectURL(isAPI, publicTS.URL+settings.RouteSubmitFlow, conf.SelfServiceFlowSettingsUI().String()))
	}

	t.Run("description=should come back with form errors if some profile data is invalid", func(t *testing.T) {
		var check = func(t *testing.T, actual string) {
			assert.NotEmpty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==csrf_token).attributes.value").String(), "%s", actual)
			assert.Equal(t, "too-short", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_long_string).attributes.value").String(), "%s", actual)
			assert.Equal(t, "bazbar", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.stringy).attributes.value").String(), "%s", actual)
			assert.Equal(t, "2.5", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.numby).attributes.value").String(), "%s", actual)
			assert.Equal(t, "length must be >= 25, but got 9", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_long_string).messages.0.text").String(), "%s", actual)
		}

		var payload = func(v url.Values) {
			v.Set("profile.traits.should_long_string", "too-short")
			v.Set("profile.traits.stringy", "bazbar")
		}

		t.Run("type=api", func(t *testing.T) {
			check(t, expectValidationError(t, true, apiUser1, payload))
		})

		t.Run("type=browser", func(t *testing.T) {
			check(t, expectValidationError(t, false, browserUser1, payload))
		})
	})

	t.Run("description=should not be able to make requests for another user", func(t *testing.T) {
		t.Run("type=api", func(t *testing.T) {
			f := testhelpers.InitializeSettingsFlowViaAPI(t, apiUser1, publicTS)

			values := testhelpers.SDKFormFieldsToURLValues(f.Ui.Nodes)
			actual, res := testhelpers.SettingsMakeRequest(t, true, f, apiUser2, testhelpers.EncodeFormAsJSON(t, true, values))
			assert.Equal(t, http.StatusBadRequest, res.StatusCode)
			assert.Contains(t, gjson.Get(actual, "ui.messages.text").String(), "initiated by another person", "%s", actual)
		})

		t.Run("type=browser", func(t *testing.T) {
			f := testhelpers.InitializeSettingsFlowViaBrowser(t, browserUser1, publicTS)

			values := testhelpers.SDKFormFieldsToURLValues(f.Ui.Nodes)
			actual, res := testhelpers.SettingsMakeRequest(t, false, f, browserUser2, values.Encode())
			assert.Equal(t, http.StatusOK, res.StatusCode)
			assert.Contains(t, gjson.Get(actual, "ui.messages.text").String(), "initiated by another person", "%s", actual)
		})
	})

	t.Run("description=should end up at the login endpoint if trying to update protected field without sudo mode", func(t *testing.T) {
		var run = func(t *testing.T, config *kratos.SettingsFlow, isAPI bool, c *http.Client) *http.Response {
			time.Sleep(time.Millisecond)

			values := testhelpers.SDKFormFieldsToURLValues(config.Ui.Nodes)
			values.Set("profile.traits.email", "not-john-doe@foo.bar")
			res, err := c.PostForm(config.Ui.Action, values)
			require.NoError(t, err)
			defer res.Body.Close()

			return res
		}

		t.Run("type=api", func(t *testing.T) {
			f := testhelpers.InitializeSettingsFlowViaAPI(t, apiUser1, publicTS)
			res := run(t, f, true, apiUser1)
			assert.EqualValues(t, http.StatusForbidden, res.StatusCode)
			assert.Contains(t, res.Request.URL.String(), publicTS.URL+settings.RouteSubmitFlow)
		})

		t.Run("type=browser", func(t *testing.T) {
			f := testhelpers.InitializeSettingsFlowViaBrowser(t, browserUser1, publicTS)
			res := run(t, f, false, browserUser1)
			assert.EqualValues(t, http.StatusUnauthorized, res.StatusCode)
			assert.Contains(t, res.Request.URL.String(), conf.Source().String(config.ViperKeySelfServiceLoginUI))
		})
	})

	t.Run("flow=fail first update", func(t *testing.T) {
		var check = func(t *testing.T, actual string) {
			assert.EqualValues(t, settings.StateShowForm, gjson.Get(actual, "state").String(), "%s", actual)
			assert.Equal(t, "1", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_big_number).attributes.value").String(), "%s", actual)
			assert.Equal(t, "must be >= 1200 but found 1", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_big_number).messages.0.text").String(), "%s", actual)
			assert.Equal(t, "foobar", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.stringy).attributes.value").String(), "%s", actual) // sanity check if original payload is still here
		}

		var payload = func(v url.Values) {
			v.Set("profile.traits.should_big_number", "1")
		}

		t.Run("type=api", func(t *testing.T) {
			check(t, expectValidationError(t, true, apiUser1, payload))
		})

		t.Run("type=browser", func(t *testing.T) {
			check(t, expectValidationError(t, false, browserUser1, payload))
		})
	})

	t.Run("flow=fail second update", func(t *testing.T) {
		var check = func(t *testing.T, actual string) {
			assert.EqualValues(t, settings.StateShowForm, gjson.Get(actual, "state").String(), "%s", actual)

			assert.Empty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_big_number).messages.0.text").String(), "%s", actual)
			assert.Empty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_big_number).attributes.value").String(), "%s", actual)

			assert.Equal(t, "short", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_long_string).attributes.value").String(), "%s", actual)
			assert.Equal(t, "length must be >= 25, but got 5", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_long_string).messages.0.text").String(), "%s", actual)

			assert.Equal(t, "this-is-not-a-number", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.numby).attributes.value").String(), "%s", actual)
			assert.Equal(t, "expected number, but got string", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.numby).messages.0.text").String(), "%s", actual)

			assert.Equal(t, "foobar", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.stringy).attributes.value").String(), "%s", actual) // sanity check if original payload is still here
		}

		var payload = func(v url.Values) {
			v.Del("profile.traits.should_big_number")
			v.Set("profile.traits.should_long_string", "short")
			v.Set("profile.traits.numby", "this-is-not-a-number")
		}

		t.Run("type=api", func(t *testing.T) {
			check(t, expectValidationError(t, true, apiUser1, payload))
		})

		t.Run("type=browser", func(t *testing.T) {
			check(t, expectValidationError(t, false, browserUser1, payload))
		})
	})

	var expectSuccess = func(t *testing.T, isAPI bool, hc *http.Client, values func(url.Values)) string {
		return testhelpers.SubmitSettingsForm(t, isAPI, hc, publicTS, values,
			http.StatusOK,
			testhelpers.ExpectURL(isAPI, publicTS.URL+settings.RouteSubmitFlow, conf.SelfServiceFlowSettingsUI().String()))
	}

	t.Run("flow=succeed with final request", func(t *testing.T) {
		conf.MustSet(config.ViperKeySelfServiceSettingsPrivilegedAuthenticationAfter, "1h")
		t.Cleanup(func() {
			conf.MustSet(config.ViperKeySelfServiceSettingsPrivilegedAuthenticationAfter, "1ns")
		})

		var check = func(t *testing.T, actual string) {
			assert.EqualValues(t, settings.StateSuccess, gjson.Get(actual, "state").String(), "%s", actual)

			assert.Empty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.numby).attributes.errors").Value(), "%s", actual)
			assert.Empty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_big_number).attributes.errors").Value(), "%s", actual)
			assert.Empty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_long_string).attributes.errors").Value(), "%s", actual)

			assert.Equal(t, 15.0, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.numby).attributes.value").Value(), "%s", actual)
			assert.Equal(t, 9001.0, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_big_number).attributes.value").Value(), "%s", actual)
			assert.Equal(t, "this is such a long string, amazing stuff!", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_long_string).attributes.value").Value(), "%s", actual)
		}

		var payload = func(newEmail string) func(v url.Values) {
			return func(v url.Values) {
				v.Set("profile.traits.email", newEmail)
				v.Set("profile.traits.numby", "15")
				v.Set("profile.traits.should_big_number", "9001")
				v.Set("profile.traits.should_long_string", "this is such a long string, amazing stuff!")
			}
		}

		t.Run("type=api", func(t *testing.T) {
			actual := expectSuccess(t, true, apiUser1, payload("not-john-doe-api@mail.com"))
			check(t, gjson.Get(actual, "flow").Raw)
		})

		t.Run("type=browser", func(t *testing.T) {
			check(t, expectSuccess(t, false, browserUser1, payload("not-john-doe-browser@mail.com")))
		})
	})

	t.Run("flow=try another update with invalid data", func(t *testing.T) {
		var check = func(t *testing.T, actual string) {
			assert.EqualValues(t, settings.StateShowForm, gjson.Get(actual, "state").String(), "%s", actual)
		}

		var payload = func(v url.Values) {
			v.Set("profile.traits.should_long_string", "short")
		}

		t.Run("type=api", func(t *testing.T) {
			check(t, expectValidationError(t, true, apiUser1, payload))
		})

		t.Run("type=browser", func(t *testing.T) {
			check(t, expectValidationError(t, false, browserUser1, payload))
		})
	})

	t.Run("description=ensure that hooks are running", func(t *testing.T) {
		var returned bool
		router := httprouter.New()
		router.GET("/return-ts", func(w http.ResponseWriter, r *http.Request, params httprouter.Params) {
			returned = true
		})
		rts := httptest.NewServer(router)
		t.Cleanup(rts.Close)

		testhelpers.SelfServiceHookSettingsSetDefaultRedirectTo(t, conf, rts.URL+"/return-ts")
		t.Cleanup(testhelpers.SelfServiceHookConfigReset(t, conf))

		f := testhelpers.InitializeSettingsFlowViaBrowser(t, browserUser1, publicTS)

		values := testhelpers.SDKFormFieldsToURLValues(f.Ui.Nodes)
		values.Set("profile.traits.should_big_number", "9001")
		res, err := browserUser1.PostForm(f.Ui.Action, values)

		require.NoError(t, err)
		defer res.Body.Close()

		body, err := ioutil.ReadAll(res.Body)
		require.NoError(t, err)
		assert.True(t, returned, "%d - %s", res.StatusCode, body)
	})

	// Update the login endpoint to auto-accept any incoming login request!
	_ = testhelpers.NewSettingsLoginAcceptAPIServer(t, adminClient, conf)

	t.Run("description=should send email with verifiable address", func(t *testing.T) {
		conf.MustSet(config.ViperKeySelfServiceVerificationEnabled, true)
		conf.MustSet(config.ViperKeyCourierSMTPURL, "smtp://foo:bar@irrelevant.com/")
		conf.MustSet(config.ViperKeySelfServiceSettingsPrivilegedAuthenticationAfter, "1h")
		t.Cleanup(func() {
			conf.MustSet(config.ViperKeySelfServiceSettingsPrivilegedAuthenticationAfter, "1ns")
			conf.MustSet(config.HookStrategyKey(config.ViperKeySelfServiceSettingsAfter, settings.StrategyProfile), nil)
		})

		var check = func(t *testing.T, actual, newEmail string) {
			assert.EqualValues(t, settings.StateSuccess, gjson.Get(actual, "state").String(), "%s", actual)
			assert.Equal(t, newEmail, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.email).attributes.value").Value(), "%s", actual)

			m, err := reg.CourierPersister().LatestQueuedMessage(context.Background())
			require.NoError(t, err)
			assert.Contains(t, m.Subject, "verify your email address")
		}

		var payload = func(newEmail string) func(v url.Values) {
			return func(v url.Values) {
				v.Set("method", settings.StrategyProfile)
				v.Set("profile.traits.email", newEmail)
			}
		}

		t.Run("type=api", func(t *testing.T) {
			newEmail := "update-verify-api@mail.com"
			actual := expectSuccess(t, true, apiUser1, payload(newEmail))
			check(t, gjson.Get(actual, "flow").String(), newEmail)
		})

		t.Run("type=browser", func(t *testing.T) {
			newEmail := "update-verify-browser@mail.com"
			actual := expectSuccess(t, false, browserUser1, payload(newEmail))
			check(t, actual, newEmail)
		})
	})

	t.Run("description=should update protected field with sudo mode", func(t *testing.T) {
		conf.MustSet(config.ViperKeySelfServiceSettingsPrivilegedAuthenticationAfter, "5m")
		t.Cleanup(func() {
			conf.MustSet(config.ViperKeySelfServiceSettingsPrivilegedAuthenticationAfter, "1ns")
		})

		var check = func(t *testing.T, newEmail string, actual string) {
			assert.EqualValues(t, settings.StateSuccess, gjson.Get(actual, "state").String(), "%s", actual)
			assert.Empty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.numby).attributes.errors").Value(), "%s", actual)
			assert.Empty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_big_number).attributes.errors").Value(), "%s", actual)
			assert.Empty(t, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.should_long_string).attributes.errors").Value(), "%s", actual)
			assert.Equal(t, newEmail, gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.email).attributes.value").Value(), "%s", actual)
			assert.Equal(t, "foobar", gjson.Get(actual, "ui.nodes.#(attributes.name==profile.traits.stringy).attributes.value").String(), "%s", actual) // sanity check if original payload is still here
		}

		var payload = func(email string) func(v url.Values) {
			return func(v url.Values) {
				v.Set("method", settings.StrategyProfile)
				v.Set("profile.traits.email", email)
			}
		}

		t.Run("type=api", func(t *testing.T) {
			email := "not-john-doe-api@mail.com"
			actual := expectSuccess(t, true, apiUser1, payload(email))
			check(t, email, gjson.Get(actual, "flow").Raw)
		})

		t.Run("type=browser", func(t *testing.T) {
			email := "not-john-doe-browser@mail.com"
			actual := expectSuccess(t, false, browserUser1, payload(email))
			check(t, email, actual)
		})
	})
}

func TestDisabledEndpoint(t *testing.T) {
	conf, reg := internal.NewFastRegistryWithMocks(t)
	conf.MustSet(config.ViperKeyDefaultIdentitySchemaURL, "file://./stub/identity.schema.json")
	testhelpers.StrategyEnable(t, conf, settings.StrategyProfile, false)

	publicTS, _ := testhelpers.NewKratosServer(t, reg)
	browserIdentity1 := newIdentityWithPassword("john-browser@doe.com")
	browserUser1 := testhelpers.NewHTTPClientWithIdentitySessionCookie(t, reg, browserIdentity1)

	t.Run("case=should not submit when profile method is disabled", func(t *testing.T) {

		t.Run("method=GET", func(t *testing.T) {
			res, err := browserUser1.Get(publicTS.URL + settings.RouteSubmitFlow)
			require.NoError(t, err)
			assert.Equal(t, http.StatusNotFound, res.StatusCode)

			b := make([]byte, 10000)
			_, _ = res.Body.Read(b)
			assert.Contains(t, string(b), "This endpoint was disabled by system administrator")
		})

		t.Run("method=POST", func(t *testing.T) {
			res, err := browserUser1.PostForm(publicTS.URL+settings.RouteSubmitFlow, url.Values{"age": {"16"}})
			require.NoError(t, err)
			assert.Equal(t, http.StatusNotFound, res.StatusCode)

			b := make([]byte, res.ContentLength)
			_, _ = res.Body.Read(b)
			assert.Contains(t, string(b), "This endpoint was disabled by system administrator")
		})
	})
}
