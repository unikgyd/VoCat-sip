# Cellular DNS availability

Public-IP lookup and HTTP/SOCKS exports use the selected original cellular
session's DNS list. DNS sockets retain that interface's route mark and device
binding; resolving a name does not use the host's default connection.

An IPv6 DNS socket can connect successfully while its queries receive no reply.
The resolver therefore races each actual DNS exchange across the bounded list
already selected for that session, including its IPv4 and IPv6 servers. It does
not select a server merely because UDP `connect` succeeded. Duplicate addresses
are excluded and at most eight configured endpoints participate.

Only replies matching the transaction, opcode and question may win. A prompt
SERVFAIL or REFUSED does not outrun another usable answer. Authoritative negative
answers retain their DNS meaning. UDP truncation retains the Go resolver's TCP
fallback and complete length-prefixed framing. The connection is private to one
resolver exchange and rejects another submission after it starts.

Context cancellation, read/write deadlines and explicit Close stop the original
queries. Close joins every remaining socket worker before the exchange finishes.
The same bound dialer applies to every candidate and to TCP fallback. No SIM,
packet handle or cellular ownership is transferred by DNS failure handling.

The native test exposed a real case in which both advertised IPv6 DNS servers
were silent while both IPv4 servers answered. The old resolver repeatedly used
the first silent server. After correction, the same condition allowed real DNS
and TLS/public-IP traffic over the original cellular IPv4 path. This is DNS
availability evidence; it does not make an unresponsive IPv6 packet path usable.
