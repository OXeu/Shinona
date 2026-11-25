import assert from 'node:assert/strict'
import test from 'node:test'

import { interpolate, isLocale, localeOptions, preferredLocale } from '../src/shared/i18n/locale.ts'
import en_US from '../src/shared/i18n/locales/en_US.json' with { type: 'json' }
import zh_CN from '../src/shared/i18n/locales/zh_CN.json' with { type: 'json' }
import zh_HK from '../src/shared/i18n/locales/zh_HK.json' with { type: 'json' }

const tokens = (message) => [...message.matchAll(/\{\w+\}/gu)].map(([token]) => token).sort()

test('all supported locales have matching, nonempty messages and interpolation tokens', () => {
  const catalogs = { en_US, zh_CN, zh_HK }
  const keys = Object.keys(en_US).sort()
  assert.deepEqual(localeOptions.map(({ value }) => value), Object.keys(catalogs))
  for (const [locale, catalog] of Object.entries(catalogs)) {
    assert.deepEqual(Object.keys(catalog).sort(), keys, locale)
    for (const [key, message] of Object.entries(catalog)) {
      assert.ok(message.trim(), `${locale}: ${key} is empty`)
      assert.deepEqual(tokens(message), tokens(en_US[key]), `${locale}: ${key}`)
    }
  }
})

test('browser languages resolve by preference, script and region', () => {
  assert.equal(preferredLocale(['fr-CA', 'en-GB', 'zh-HK']), 'en_US')
  assert.equal(preferredLocale(['zh-HK', 'zh-CN']), 'zh_HK')
  assert.equal(preferredLocale(['zh-Hant', 'en-US']), 'zh_HK')
  assert.equal(preferredLocale(['zh-TW']), 'zh_HK')
  assert.equal(preferredLocale(['yue-HK']), 'zh_HK')
  assert.equal(preferredLocale(['zh-Hans-HK']), 'zh_CN')
  assert.equal(preferredLocale(['zh_CN']), 'zh_CN')
  assert.equal(preferredLocale(['zh']), 'zh_CN')
  assert.equal(preferredLocale(['@@', 'fr-FR']), 'en_US')
  assert.equal(preferredLocale([]), 'en_US')
  assert.equal(isLocale('zh_HK'), true)
  assert.equal(isLocale('zh-HK'), false)
  assert.equal(isLocale('toString'), false)
})

test('interpolation preserves user-supplied content and accepts numbers', () => {
  assert.equal(interpolate(zh_HK['sessions.showAll'], { count: 0 }), '顯示全部 0 項')
  assert.equal(
    interpolate(zh_CN['sessions.archiveConfirm'], { title: '{custom} & 日志' }),
    '归档“{custom} & 日志”并从导航中隐藏？',
  )
})
