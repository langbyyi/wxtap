package main

// WxTap's fixed loopback port defaults. 9421 is owned by the WMPF runtime and
// remains fixed; these services intentionally live in a separate namespace
// from that WMPF debug channel.
const (
	defaultCDPPort      = 31415
	defaultCloudAPIPort = 27182
	defaultMCPPort      = 9527
)
