import assert from "node:assert/strict";
import { after, before, test } from "node:test";

import { ConfigError, Gateway, gatewayUrl, organisationOfKey, RpcError } from "../src/gateway.ts";
import { FakeGateway } from "./fakes.ts";

let gw: FakeGateway;
before(async () => {
  gw = await new FakeGateway().start();
});
after(() => gw.close());

const KEY = "organisations/acme/apiKeys/k1";

test("a call presents the key and its secret, and returns the reply only on a status of zero", async () => {
  gw.replies.push({ message: new Uint8Array([8, 1]) });
  const reply = await new Gateway(gw.url, KEY, "s3cret").call("/svc/Method", new Uint8Array([1, 2, 3]), 2000);
  assert.deepEqual([...reply], [8, 1]);
  const call = gw.calls.at(-1)!;
  assert.equal(call.headers["x-api-key"], KEY);
  assert.equal(call.headers["x-api-secret"], "s3cret");
  assert.equal(call.headers["content-type"], "application/grpc-web+proto");
  assert.deepEqual([...call.message], [1, 2, 3]);
});

test("a refusal in the trailer, in the headers, or no status at all is an error by its code", async () => {
  const g = new Gateway(gw.url, KEY, "s");
  gw.replies.push({ trailerStatus: 7 });
  await assert.rejects(g.call("/m", new Uint8Array(), 2000), (e: RpcError) => e.codeName === "PermissionDenied");
  gw.replies.push({ headerStatus: 16 });
  await assert.rejects(g.call("/m", new Uint8Array(), 2000), (e: RpcError) => e.codeName === "Unauthenticated");
  gw.replies.push({ trailerStatus: null });
  await assert.rejects(g.call("/m", new Uint8Array(), 2000), (e: RpcError) => e.codeName === "Internal");
  gw.replies.push({ httpStatus: 503 });
  await assert.rejects(g.call("/m", new Uint8Array(), 2000), (e: RpcError) => e.codeName === "Unavailable");
});

test("a gateway too slow to answer is a deadline", async () => {
  gw.replies.push({ delayMs: 500 });
  await assert.rejects(new Gateway(gw.url, KEY, "s").call("/m", new Uint8Array(), 50), (e: RpcError) => e.codeName === "DeadlineExceeded");
});

test("a key never travels in clear anywhere but this machine", () => {
  assert.throws(() => gatewayUrl("http://gateway.example.com"), ConfigError);
  assert.equal(gatewayUrl("gateway.example.com/"), "https://gateway.example.com");
  assert.equal(gatewayUrl("http://127.0.0.1:9000"), "http://127.0.0.1:9000");
});

test("the organisation is read off the key's name", () => {
  assert.equal(organisationOfKey(KEY), "organisations/acme");
  assert.equal(organisationOfKey("just-a-secret"), "");
  assert.equal(organisationOfKey("organisations/a/b/apiKeys/k"), "");
});

test("a missing setting stops the recorder where it is set up", () => {
  assert.throws(() => new Gateway("", KEY, "s"), ConfigError);
  assert.throws(() => new Gateway("gw", "", "s"), ConfigError);
  assert.throws(() => new Gateway("gw", KEY, " "), ConfigError);
});
