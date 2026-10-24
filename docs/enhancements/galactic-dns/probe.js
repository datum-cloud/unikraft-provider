// Run only in a disposable guest: this probe replaces /etc/resolv.conf.
const fs = require('fs');
const dns = require('dns');
const net = require('net');
const os = require('os');

const out = (tag, value) => console.log(JSON.stringify({ tag, value }));

async function attempt(tag, fn) {
  try {
    out(tag, await Promise.race([
      fn(),
      new Promise((_, reject) => setTimeout(() => reject(Error('timeout')), 12000)),
    ]));
  } catch (e) {
    out(tag, { error: e.message, code: e.code });
  }
}

(async () => {
  for (const path of ['/etc/resolv.conf', '/proc/cmdline', '/proc/net/ipv6_route']) {
    try {
      out(path, fs.readFileSync(path, 'utf8'));
    } catch (e) {
      out(path, e.message);
    }
  }
  out('interfaces', os.networkInterfaces());
  await attempt('default-lookup-before', () => dns.promises.lookup('example.com', { family: 6 }));

  const resolver = new dns.promises.Resolver({ timeout: 3000, tries: 1 });
  resolver.setServers(['2606:4700:4700::1111']);
  await attempt('cloudflare-udp-AAAA', () => resolver.resolve6('example.com'));
  await attempt('cloudflare-tcp-AAAA', () => new Promise((resolve, reject) => {
    // Recursive example.com IN AAAA query, with the DNS-over-TCP length prefix.
    const query = Buffer.from('a12301000001000000000000076578616d706c6503636f6d00001c0001', 'hex');
    const length = Buffer.alloc(2);
    length.writeUInt16BE(query.length);
    const socket = net.connect(
      { host: '2606:4700:4700::1111', port: 53, family: 6 },
      () => socket.write(Buffer.concat([length, query])),
    );
    let response = Buffer.alloc(0);
    socket.on('data', chunk => {
      response = Buffer.concat([response, chunk]);
      if (response.length >= 2 && response.length >= response.readUInt16BE(0) + 2) {
        resolve({
          rcode: response[5] & 15,
          answers: response.readUInt16BE(8),
          bytes: response.length,
        });
        socket.end();
      }
    });
    socket.on('error', reject);
    socket.setTimeout(6000, () => socket.destroy(Error('TCP timeout')));
  }));

  // Remove this block when validating DNS supplied by a startup initializer.
  try {
    fs.writeFileSync('/etc/resolv.conf',
      'nameserver 2606:4700:4700::1111\noptions timeout:2 attempts:1\n');
    out('resolv-after', fs.readFileSync('/etc/resolv.conf', 'utf8'));
  } catch (e) {
    out('write-error', e.message);
  }

  await attempt('default-lookup-after', () => dns.promises.lookup('example.com', { family: 6 }));
  await attempt('https-after', async () => {
    const response = await fetch('https://example.com', { signal: AbortSignal.timeout(8000) });
    return { status: response.status };
  });
  out('done', true);
  // Leave the guest available for inspection until the test Pod is deleted.
  setInterval(() => {}, 60000);
})();
