# Proxy Setup Guide

This script includes built-in proxy support for running multiple network connections and rotating through proxy IPs to distribute the load and avoid rate limits.

## Proxy Formats

The script supports two proxy formats:

### 1. Single Proxy Line Format
```
PROXY_HOST:PROXY_PORT
PROXY_HOST:PROXY_PORT:USERNAME:PASSWORD
```

**Examples:**
- `proxy.example.com:8080`
- `user:pass@proxy.example.com:8080`

### 2. Proxy File Format
Create a text file with one proxy per line:
```
proxy1.example.com:8080
proxy2.example.com:8080:user:pass
proxy3.example.com:8080:pass2:user2
```

## Proxy Usage in Script

### Command Line Arguments

#### Single Proxy (for individual runs)
```bash
# For fetch command
userkarmacheck fetch username -p proxy.example.com:8080

# For fetchhtml command
userkarmacheck fetchhtml username -p proxy.example.com:8080
```

#### Proxy File (for multi-proxy runs)
```bash
# Pass proxy file with --proxies flag
userkarmacheck check --sessions sassion --proxies proxies.txt
```

### In run.bat

#### Manual Proxy Addition
```bat
"%EXE%" check -i "%~dp0input.txt" -o "%~dp0full_out" ^
    --sessions "%~dp0sassion" --lanes 466 --threads 150 ^
    --rate-per-ip 1200 --rate-per-session 72 --warmup 40 ^
    --proxies myproxies.txt  ^
    --browser-pages 4 --browser-budget 60 ^
    --vpn-rotate --vpn-network-lock
```

## Multi-Proxy Architecture

### How It Works

1. **Per-Proxy Client Pool**
   - Each proxy gets its own HTTP client instance
   - Clients are reused throughout the run for performance
   - Automatic reconnection if a proxy fails

2. **Smart Rotation**
   - Workers rotate through proxy list when rate limiting occurs
   - Failed proxies are automatically skipped
   - Load is distributed across all available proxies

3. **Rate Limiting per Proxy**
   - Each proxy has its own rate limits
   - Independent tracking of blocks and throttles
   - Better fault isolation

### Command Line Arguments for Multi-Proxy

```bash
# Basic multi-proxy run
userkarmacheck check --sessions sassion --proxies proxies.txt

# With proxy-specific options
userkarmacheck check --sessions sassion --proxies proxies.txt \
    --lanes 300 --threads 150 \
    --rate-per-ip 1200 --rate-per-session 72 \
    --browser-pages 4 --browser-budget 60
```

### Lane Pool Configuration

The script creates:
- **One lane per session**
- **Multiple sessions per proxy** (based on `--lanes` setting)
- **Workers share proxies** through round-robin distribution

Example with 300 proxies and 300 lanes:
```
--proxies proxies.txt  # 300 proxies
--lanes 300           # 300 lanes (1 lane per proxy)
--threads 150          # 150 concurrent workers
```

## Proxy Health Management

### Failure Handling

1. **Connection Failures**
   - Automatically detected and logged
   - Failed proxies are skipped in rotation
   - Script continues with remaining proxies

2. **Rate Limit Recovery**
   - Circuit breaker resets for each proxy individually
   - After a block, the proxy cooldown timer starts
   - Other proxies continue working normally

3. **Authentication Issues**
   - Invalid proxy credentials are detected
   - Script skips bad proxies and continues
   - Logs error for manual intervention

### Health Monitoring

The script tracks:
- **Successful requests per proxy**
- **Failure rate per proxy**
- **Block/stale status per proxy**
- **Circuit breaker state per proxy**

## Public Proxy Sources

### Free Proxy Lists

#### 1. Free Proxy List
```bash
# Download from: https://free-proxy-list.net/
# OR use direct API:
curl -s "https://free-proxy-list.net/" | grep -E "([0-9]{1,3}\.){3}[0-9]{1,3}" > proxies.txt
```

#### 2. ProxyScrape
```bash
# Get rotating proxies from: https://api.proxyscrape.com/
curl "https://api.proxyscrape.com/?url=https://reddit.com&format=json&key=YOUR_KEY" > proxies.json
```

#### 3. GitHub Repositories
```bash
# Free and Fast Proxies (GitHub)
curl -s "https://raw.githubusercontent.com/clarketmm/proxy-list/master/proxy-list-raw.txt" > proxies.txt
```

### Proxy Validation Script

Here's a simple script to validate proxy connectivity:
```bash
#!/bin/bash
# proxy_validator.sh - Test proxy connectivity

TIMEOUT=10
ERROR_PROXY="error_proxy.txt"
VALID_PROXY="valid_proxies.txt"

while read proxy; do
    # Skip empty lines and comments
    [[ -z "$proxy" || "$proxy" == \#* ]] && continue

    echo "Testing proxy: $proxy"

    # Quick connectivity test
    if timeout $TIMEOUT curl -s --proxy "http://$proxy" "https://www.reddit.com" > /dev/null 2>&1; then
        echo "$proxy" >> $VALID_PROXY
        echo "✓ Proxy $proxy is working"
    else
        echo "$proxy" >> $ERROR_PROXY
        echo "✗ Proxy $proxy failed"
    fi
done < proxies.txt

echo "Validation complete:"
echo "Valid proxies saved to: $VALID_PROXY"
echo "Failed proxies saved to: $ERROR_PROXY"
```

## Advanced Proxy Configuration

### Proxy Authentication

```bash
# Authenticated proxy example
userkarmacheck check --sessions sassion --proxies proxies.txt

# Where proxies.txt contains:
proxy1.example.com:8080:username:password
proxy2.example.com:8080:user2:pass2
```

### SOCKS5 Proxies

Note: The script primarily supports HTTP/HTTPS proxies. For SOCKS5, you may need to convert them:

```bash
# Convert SOCKS5 to HTTP format (if supported by your target)
# This is just an example - check your proxy provider documentation
socks5proxy:1080:user:pass
# May need to be converted to:
http://user:pass@socks5proxy:1080
```

## Proxy Error Messages

### Common Issues and Solutions

1. **Connection Refused**
   ```
   ERROR: proxy connection failed
   ```
   **Solution:** Check if proxy is accessible, ensure proxy port is open

2. **Authentication Failed**
   ```
   ERROR: proxy authentication failed
   ```
   **Solution:** Verify proxy username/password, check proxy authentication type

3. **Timeout**
   ```
   ERROR: proxy connection timeout
   ```
   **Solution:** Increase timeout, check proxy performance

4. **Invalid Proxy Format**
   ```
   ERROR: invalid proxy format
   ```
   **Solution:** Check proxy format: `host:port` or `host:port:user:pass`

## Proxy Best Practices

### 1. Proxy Rotation Strategy
```bash
# Use different proxy types for different stages
userkarmacheck check --sessions sassion --proxies residential_proxies.txt --lanes 200
userkarmacheck check --sessions sassion --proxies datacenter_proxies.txt --lanes 200
```

### 2. Proxy Backup Plan
```bash
# Have backup proxies in case primary ones fail
userkarmacheck check --sessions sassion --proxies main_proxies.txt --proxies backup_proxies.txt
```

### 3. Proxy Monitoring
```bash
# Script to monitor proxy health
#!/bin/bash
while true; do
    # Monitor proxy success rate
    success_rate=$(calculate_success_rate)
    if [ $success_rate -lt 80 ]; then
        echo "WARNING: Proxy success rate too low: $success_rate%"
    fi
    sleep 300
 done
```

## Technical Details

### Proxy Configuration

#### Client Creation
```go
// From fetch.go
func newClient(proxyLine string, timeout time.Duration) (*Client, error) {
    if p := ParseProxy(proxyLine); p != "" {
        tr := &http.Transport{
            Proxy: func(*http.Request) (*url.URL, error) {
                return url.Parse(p)
            },
        }
        // Configure timeout and TLS
    }
    // ... rest of client configuration
}
```

### Rate Limiting with Proxies

Each proxy maintains independent:
- **Token buckets** for HTTP requests
- **Circuit breakers** for blocks
- **AIMD controllers** for rate adjustment

This means:
1. **Proxy isolation:** If one proxy gets blocked, others continue
2. **Better distribution:** Load is spread across all proxies
3. **Faster recovery:** Unblocked proxies work at full rate

## Troubleshooting

### Common Issues and Solutions

1. **All proxies failing**
   - Check if proxies.txt file exists
   - Verify proxy format
   - Test manually with curl

2. **No requests going through proxies**
   - Check --proxies flag syntax
   - Verify proxy file format
   - Check if workers are rotating properly

3. **Some proxies working, others failing**
   - Check proxy server status
   - Verify authentication credentials
   - Test individual proxies

### Debug Proxy Usage

Add debug logging to see which proxy is being used:
```bash
# Set debug environment variable
KARMA_DEBUG=1 userkarmacheck check --sessions sassion --proxies proxies.txt
```

Or modify the script to log proxy usage:
```go
// In clientFor function (worker.go)
func (ch *Checker) clientFor(proxyLine string) (*Client, error) {
    if c, ok := ch.clients[proxyLine]; ok {
        return c, nil
    }
    // Create new client for this proxy
    // Add logging here
}
```

## Performance Optimization

### Proxy Performance Tips

1. **Warm up proxies** before main run
2. **Use geographically diverse proxies**
3. **Monitor proxy latency**
4. **Set appropriate timeout values**
5. **Implement proxy health checks**

### Example Performance Script
```bash
#!/bin/bash
# proxy_performance_test.sh - Test proxy performance

TIMEOUT=30
MIN_SUCCESS_RATE=90

while read proxy; do
    # Skip empty lines and comments
    [[ -z "$proxy" || "$proxy" == \#* ]] && continue

    echo "Testing proxy performance: $proxy"

    # Measure response time
    start_time=$(date +%s)
    if timeout $TIMEOUT curl -s --proxy "http://$proxy" -w "%{time_total}" "https://www.reddit.com" > /tmp/proxy_test_response.txt 2>&1; then
        end_time=$(date +%s)
        response_time=$(cat /tmp/proxy_test_response.txt)
        success_rate=$(calculate_success_rate)

        if [ $(echo "$success_rate < $MIN_SUCCESS_RATE" | bc -l) -eq 1 ]; then
            echo "WARNING: Proxy $proxy has low success rate: $success_rate%"
        fi

        echo "✓ Proxy $proxy response time: ${response_time}s success rate: $success_rate%"
    else
        echo "✗ Proxy $proxy failed: $(cat /tmp/proxy_test_response.txt)"
    fi
done < proxies.txt
```

## Summary

The script provides comprehensive proxy support with:

1. **Multiple proxy formats** (single proxy or proxy file)
2. **Smart rotation** (workers automatically rotate through proxies)
3. **Independent rate limiting** (each proxy has its own limits)
4. **Automatic failure handling** (failed proxies are skipped)
5. **Health monitoring** (tracks proxy success rates)
6. **Configurable authentication** (supports username/password)
7. **Performance optimization** (client pooling, load balancing)

To get started:
1. Get a proxy list (use free proxy sites)
2. Format it correctly
3. Run with --proxies flag
4. Monitor results and adjust as needed

The script will automatically handle most proxy-related issues, making it suitable for both beginners and advanced users.