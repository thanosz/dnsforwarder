# DNS Forwarder

`dnsforwarder` is a Go-based application designed to function as a DNS forwarding and caching server for macOS. It leverages macOS’s `/etc/resolver` directory to redirect DNS queries to a locally running DNS server. This setup is particularly useful for handling DNS resolution when using multiple VPN connections. For further details on custom DNS resolvers in macOS, you can refer to [this guide](https://vninja.net/2020/02/06/macos-custom-dns-resolvers/).

## Background

When connected to multiple VPN networks, the last active VPN usually overrides the DNS settings, which can cause issues with resolving domain names on other VPN networks.

For example, let’s say you're connected to your company’s VPN (Company A) with DNS server `10.1.1.1`. If you then connect to another company’s VPN (Company B), which uses DNS server `172.1.1.1`, your DNS queries will now be handled by the DNS of Company B. While you may still have access to Company A’s network, any DNS queries for Company A's internal resources will fail since they won’t be directed to the `10.1.1.1` DNS server.

This is where `dnsforwarder` comes in—by allowing you to forward specific domain queries to designated DNS servers based on the VPN you're connected to.

## How It Works

The `dnsforwarder` tool resolves this problem by allowing you to define a list of DNS servers and the corresponding domains they should serve. It does this by:

1. **Starting a local DNS server** on `127.0.0.1:53` (the standard loopback interface and DNS port).
2. **Creating domain-specific configuration files** under the `/etc/resolver` directory, each pointing to the local DNS server (`127.0.0.1`).
3. **Parsing the system’s `/etc/resolv.conf` file**, which contains the system’s current DNS configuration.
4. **Building a list of DNS servers** to try, starting with the custom servers you’ve provided, followed by the system-set DNS servers.
5. **Removing the `/etc/resolver` files** upon exiting, ensuring a clean state.

## Execution Details

When you run `dnsforwarder`, the following happens:

- **Matching domains**: If a DNS query matches any of the domains you specified, macOS will forward the query to the local DNS server (`127.0.0.1`), based on the entries in the `/etc/resolver` directory.
- **Cache check**: The `dnsforwarder` will first check its cache to see if it already has the result for that query.
- **Custom DNS servers**: If the query isn’t in the cache, it will try the custom DNS servers you’ve specified.
- **Fallback to system DNS**: If the custom DNS servers don’t have the answer, it will then query the system DNS servers from `/etc/resolv.conf`.
- **Caching results**: Once a DNS query is successfully resolved, the result will be cached for 10 minutes to optimize future lookups.
- **Automatic updates**: `/etc/resolv.conf` file is monitored for updates (for example, when you switch VPN connections). `dnsforwarder` automatically detects the changes and updates its list of DNS servers accordingly.

### Example Usage

Let’s say you want to forward all DNS queries for `companya.com` and `companya-resources.com` to the DNS server `10.1.1.1` (used by Company A), while still using the system-set DNS servers if the custom DNS server fails. You would run:

```bash
sudo ./dnsforwarder --servers 10.1.1.1 --domains companya.com,companya-resources.com
```

This command ensures that DNS queries for Company A’s domains are directed to 10.1.1.1, but it will fall back to the system’s DNS configuration if necessary.

## Notes

- Since `dnsforwarder` needs to create and delete files in the system’s `/etc/resolver` directory, it requires sudo to run.
- You can set the system DNS server to 127.0.0.1 from the MacOS settings. In this case `dnsforwarder` will use 1.1.1.1 as the system-set DNS server



