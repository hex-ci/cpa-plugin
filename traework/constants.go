// constants.go holds the stable identifiers of the TraeWork provider: the
// plugin's own name (config keys, routes, auth files) and the upstream endpoints
// the CN desktop client of TraeWork 0.1.52 uses.
package main

import "time"

const (
	providerName = "traework"
	// displayName is the human-readable plugin title; providerName stays the
	// stable identifier used for config keys, routes and auth file names.
	displayName   = "TraeWork"
	authFileName  = "traework.json"
	pluginLogoURL = "https://raw.githubusercontent.com/hex-ci/cpa-plugin/main/traework/assets/traework.png"

	// CN account API (iss = trae.cn realm): OAuth token exchange and profile.
	apiBaseCN = "https://api.trae.cn"
	// Browser authorization page. It validates auth_callback_url and only
	// accepts a 127.0.0.1 callback whose path is /authorize (see oauth.go).
	authorizeBase = "https://www.trae.cn/authorization"

	// OAuth surface on apiBaseCN: the code exchange (also used for refresh)
	// and the profile read.
	oauthExchangePath = "/trae/api/v3/oauth/ExchangeToken"
	oauthUserInfoPath = "/cloudide/api/v3/trae/GetUserInfo"
	// The refresh call is the same ExchangeToken endpoint with KeyInfoType
	// refresh_token (see refreshAccessToken).
	oauthClientID = "en1oxy7wnw8j9n"

	// CN agent gateway: the "solo" function channel serves chat and the model
	// catalogue. Both are POST endpoints on this host.
	soloAPIBase = "https://trae-api-cn.mchost.guru"
	// soloChatPath is the streaming chat endpoint (function=solo_work_lite).
	soloChatPath = "/api/agent/v3/llm_utils_chat"
	// soloCatalogPath returns the account's model catalogue; the chat request
	// selects a model by its config_name.
	soloCatalogPath = "/api/ide/v1/get_detail_param"
	// soloFunction is the only channel this plugin serves.
	soloFunction = "solo_work_lite"

	// catalogCacheTTL bounds how long a fetched catalogue is reused.
	catalogCacheTTL = 30 * time.Minute

	// Fixed IDE identity reported to the agent gateway. The server routes on
	// these headers, not on the token's contents.
	appID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	ideVersion     = "0.1.69"
	ideVersionCode = "20260811"
	appType        = "SOLO"
	ideUserAgent   = "Trae/" + ideVersion
	osVersion      = "10.0.19045"
	deviceType     = "Windows"
	deviceBrand    = "83DG"

	// defaultManagementBasePath / defaultResourceBasePath are the host's
	// historical layout, used until ManagementRegister reports the real one.
	defaultManagementBasePath = "/v0/management"
	defaultResourceBasePath   = "/v0/resource/plugins/" + providerName

	// loginTTL is how long an attempt waits for the browser. The desktop client
	// uses 5 minutes for its own interactive window; a management-UI login is
	// driven by hand, so this waits as long as the authorization code itself
	// stays redeemable (10 minutes). The listener closes as soon as the code
	// lands, so the port is not held for the whole window.
	loginTTL = 10 * time.Minute
	// codeGrace keeps an attempt alive after the browser delivered its code:
	// the authorization code itself is valid for 10 minutes, so a poll that
	// arrives late still gets the credential instead of a timeout.
	codeGrace = 10 * time.Minute
)
