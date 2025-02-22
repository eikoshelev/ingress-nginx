local conf = [===[
nameserver 1.2.3.4
nameserver 4.5.6.7
search ingress-nginx.svc.cluster.local svc.cluster.local cluster.local
options ndots:5
]===]

helpers.with_resolv_conf(conf, function()
  require("util.resolv_conf")
end)

describe("resolve", function()
  local dns = require("util.dns")

  it("sets correct nameservers", function()
    helpers.mock_resty_dns_new(function(self, options)
      assert.are.same({ nameservers = { "1.2.3.4", "4.5.6.7" }, retrans = 5, timeout = 2000 }, options)
      return nil, ""
    end)
    dns.resolve("example.com")
  end)

  it("returns host when an error happens", function()
    local s_ngx_log = spy.on(ngx, "log")

    helpers.mock_resty_dns_new(function(...) return nil, "an error" end)
    assert.are.same({ "example.com" }, dns.resolve("example.com"))
    assert.spy(s_ngx_log).was_called_with(ngx.ERR, "failed to instantiate the resolver: an error")

    helpers.mock_resty_dns_query(nil, "oops!")
    assert.are.same({ "example.com" }, dns.resolve("example.com"))    
    assert.spy(s_ngx_log).was_called_with(ngx.ERR, "failed to query the DNS server:\noops!\noops!")

    helpers.mock_resty_dns_query({ errcode = 1, errstr = "format error" })
    assert.are.same({ "example.com" }, dns.resolve("example.com"))
    assert.spy(s_ngx_log).was_called_with(ngx.ERR, "failed to query the DNS server:\nserver returned error code: 1: format error\nserver returned error code: 1: format error")

    helpers.mock_resty_dns_query({})
    assert.are.same({ "example.com" }, dns.resolve("example.com"))
    assert.spy(s_ngx_log).was_called_with(ngx.ERR, "failed to query the DNS server:\nno record resolved\nno record resolved")

    helpers.mock_resty_dns_query({ { name = "example.com", cname = "sub.example.com", ttl = 60 } })
    assert.are.same({ "example.com" }, dns.resolve("example.com"))
    assert.spy(s_ngx_log).was_called_with(ngx.ERR, "failed to query the DNS server:\nno record resolved\nno record resolved")
  end)

  it("resolves all A records of given host, caches them with minimal ttl and returns from cache next time", function()
    helpers.mock_resty_dns_query({
      {
        name = "example.com",
        address = "192.168.1.1",
        ttl = 3600,
      },
      {
        name = "example.com",
        address = "1.2.3.4",
        ttl = 60,
      }
    })

    local lrucache = require("resty.lrucache")
    local old_lrucache_new = lrucache.new
    lrucache.new = function(...)
      local cache = old_lrucache_new(...)

      local old_set = cache.set
      cache.set = function(self, key, value, ttl)
        assert.equal("example.com", key)
        assert.are.same({ "192.168.1.1", "1.2.3.4" }, value)
        assert.equal(60, ttl)
        return old_set(self, key, value, ttl)
      end

      return cache
    end

    assert.are.same({ "192.168.1.1", "1.2.3.4" }, dns.resolve("example.com"))

    helpers.mock_resty_dns_new(function(...)
      error("expected to short-circuit and return response from cache")
    end)
    assert.are.same({ "192.168.1.1", "1.2.3.4" }, dns.resolve("example.com"))
  end)
end)
