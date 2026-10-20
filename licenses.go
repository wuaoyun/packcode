package main

import _ "embed"

// Keep notices inside the single executable as well as in the source tree.
//
//go:embed THIRD_PARTY_NOTICES.txt
var thirdPartyNotices string
