// Package shoutrrr implements the shoutrrr target type: one send per
// message through a service of github.com/nicholas-fedor/shoutrrr, chosen
// by the scheme of the URL. A build with the noshoutrrr tag leaves the
// library out, and New then rejects every URL.
package shoutrrr
