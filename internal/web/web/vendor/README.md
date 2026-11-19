# Vendored, not depended on

`hls.light.min.js` is hls.js 1.6.15, Apache-2.0, from
<https://github.com/video-dev/hls.js>, copied here and compiled into the
binary with the rest of the page.

It is here because a browser that is not Safari cannot play an HLS stream on
its own, and HLS is how a transcode reaches a player while it is still being
made. The "light" build is the one without the alternate-audio and subtitle
machinery, which this server does not produce.

Vendored rather than fetched, for the same reason everything else about BMS is
one file: a release copied to a machine has to work there, and a page that
reaches out to a CDN is a page that breaks on a server with no outbound
internet — which is most of the ones this is written for. Updating it means
replacing this file and the licence beside it, and nothing else.
