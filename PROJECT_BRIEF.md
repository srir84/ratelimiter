# Project brief

- We will build a rate limiter from scratch to control how many requests can pass through over time. We will test whether it enforces its limits under traffic that resembles real usage.
- The limiter is the product. A simple HTTP API and database are its test harness: each GET request captures its arrival time and calls the limiter.
- If the request is allowed, the API will store that timestamp in the database and return it with a `200` response. If the limit is exceeded, it will return `429` without writing to the database.
- Timestamps will use nanoseconds, although the clock's actual precision depends on the machine.
- A traffic script will simulate users with different activity levels, irregular requests, sudden bursts, and quiet periods. Requests will arrive at scheduled times, independently of earlier responses.
- We will repeat the same traffic pattern with and without the limiter. Request counts, response times, errors, and database writes will show whether the limiter works correctly and reduces database load.
- We will start with a configurable token bucket and one per-user budget shared across three API processes on the laptop. The final technology choices and measured capacity will follow in the design and testing stages.

## MUST have

- **Clear limits.** Define how much traffic is allowed over time, whether bursts are permitted, and which settings can be changed. The policy must be precise enough to test.
- **Defined identity and scope.** Decide which requests share a budget, such as requests from one user or to the whole service. Separate budgets must not consume each other's allowance.
- **An algorithm that matches the policy.** Choose and implement the algorithm that enforces our rules. Its behavior during bursts and at time boundaries must be understood and tested.
- **Reliable timing.** Inject a shared clock where atomic state operations execute, and substitute controlled time in tests. Budget and expiry time must not go backwards. Keep it separate from database arrival timestamps.
- **Safe state and cleanup.** Bound memory use and require cleanup. Keep receipts with callers; store only budget metadata and temporary refund markers. Expired receipts must stay unusable after cleanup or bucket recreation.
- **Atomic decisions.** Check and consume allowance as one protected operation. Simultaneous requests must not spend the same allowance or bypass the limit.
- **Enforcement before database work.** Check every request before inserting its timestamp. Quota rejections must return `429` with `Retry-After` and make no database write.
- **Explicit deployment scope.** State whether budgets apply to one process or are shared across servers. If shared budgets are supported, every server must spend from the same budget.
- **Predictable failures.** All instances must agree on the failure policy. Configure refusal or bypass only when acquisition definitely did not happen; a possibly charged request returns an unknown outcome. Keep capacity failures distinct from quota rejection.
- **Fair refunds.** A normal allowed call returns a signed receipt with an expiry. Provide enough refund capacity for the supported failure load and complete confirmed server-failure refunds within the configured deadlines. The limiter does not inspect the application database or recover unknown crashes.
- **Visible results.** Count allowed requests, rejections, and errors. Measure time spent in the limiter separately from API response time and database writes.
- **Repeatable verification.** Test bursts, refill or expiry, concurrent requests, separate budgets, and recovery. During traffic simulation, compare planned arrivals with requests actually sent and report missed arrivals or scheduling delays.
