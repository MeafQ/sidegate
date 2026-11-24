package runtime

import "syscall"

// dumpLinks asks rtnetlink for every link in one dump, with no tool.
func dumpLinks() (map[string]linkSample, error) {
	dump, err := syscall.NetlinkRIB(syscall.RTM_GETLINK, syscall.AF_UNSPEC)
	if err != nil {
		return nil, err
	}
	return parseLinkDump(dump)
}
