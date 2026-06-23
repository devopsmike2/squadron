// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// armTokenResponse is the JSON shape returned by the Azure AD token
// endpoint when the client_credentials grant succeeds.
//
//	POST https://login.microsoftonline.com/{tenant_id}/oauth2/v2.0/token
//	  grant_type=client_credentials
//	  client_id=...
//	  client_secret=...
//	  scope=https://management.azure.com/.default
//
// Only AccessToken is load-bearing for slice 1 — the scan runs for
// well under any plausible expires_in window, so the scanner does
// not track expiry. Slice 2's scheduled-scan engine will respect
// ExpiresIn.
type armTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// armTokenError is the JSON shape returned by the Azure AD token
// endpoint on a 4xx. Surfaced into the scanner's classifyTokenError
// so the operator-visible PartialReason / hard error names whether
// the failure is tenant-related, credential-related, or scope-
// related.
type armTokenError struct {
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// armVMListResponse is the JSON shape returned by the subscription-
// wide Virtual Machines list call. Only the fields slice 1 actually
// reads are typed; the SDK exposes dozens of other VM properties
// that the proposer does not reason about today.
type armVMListResponse struct {
	Value    []armVirtualMachine `json:"value"`
	NextLink string              `json:"nextLink,omitempty"`
}

// armVirtualMachine is the bare JSON shape of a single VM in the
// list response. The fields mirror the Microsoft.Compute virtual
// machine resource shape at api-version 2024-07-01 (see
// docs/proposals/azure-discovery-slice1.md §9). The slice 1
// projection reads Name, Location, Tags, and three properties
// fields; nothing else is load-bearing.
type armVirtualMachine struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Location   string             `json:"location"`
	Tags       map[string]string  `json:"tags,omitempty"`
	Properties armVirtualMachineP `json:"properties"`
}

// armVirtualMachineP is the bare JSON shape of the VM properties
// sub-object. Slice 1 reads hardwareProfile.vmSize and
// storageProfile.osDisk.osType.
type armVirtualMachineP struct {
	HardwareProfile armHardwareProfile `json:"hardwareProfile"`
	StorageProfile  armStorageProfile  `json:"storageProfile"`
}

// armHardwareProfile carries the slice 1 instance-type signal.
// Example VMSize values: "Standard_D4s_v3", "Standard_B2ms".
type armHardwareProfile struct {
	VMSize string `json:"vmSize"`
}

// armStorageProfile carries the OS family signal. Azure exposes
// osType cleanly in the same response as the VM list (unlike AWS and
// GCP slice 1), so the scanner gets OSFamily for free.
type armStorageProfile struct {
	OSDisk armOSDisk `json:"osDisk"`
}

// armOSDisk carries the operating system family — "Linux" or
// "Windows" per the Azure API enum. The slice 1 normalizeOSType
// helper maps the raw value into the lowercase ComputeInstanceSnapshot
// OSFamily field ("linux" / "windows" / "unknown").
type armOSDisk struct {
	OSType string `json:"osType"`
}

// armErrorResponse is the JSON shape Azure ARM returns on a 4xx /
// 5xx error. The scanner reads .Error.Code to disambiguate
// permission_denied ("AuthorizationFailed") vs subscription_not_found
// ("SubscriptionNotFound") vs everything else. Mirrors the GCP
// scanner's googleapi.Error pattern.
type armErrorResponse struct {
	Error armErrorBody `json:"error"`
}

type armErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
