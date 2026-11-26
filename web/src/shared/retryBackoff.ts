const retryDelays = [1_000, 2_000, 4_000, 8_000, 16_000, 30_000]

export class RetryBackoff {
  private failures = 0

  next(): number | undefined {
    const delay = retryDelays[this.failures]
    if (delay !== undefined) this.failures++
    return delay
  }

  reset() {
    this.failures = 0
  }
}
