import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { applyTheme, storedTheme, useTheme, useThemeChoice } from './theme';

beforeEach(() => {
  localStorage.clear();
  delete document.documentElement.dataset.theme;
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe('storedTheme', () => {
  it('defaults to dark', () => {
    expect(storedTheme()).toBe('dark');
  });

  it('returns a remembered light choice', () => {
    localStorage.setItem('nanocloud.theme', 'light');
    expect(storedTheme()).toBe('light');
  });

  it('falls back to dark when storage is unavailable', () => {
    vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
      throw new Error('SecurityError');
    });
    expect(storedTheme()).toBe('dark');
  });
});

describe('useTheme', () => {
  it('switches the theme on <html> and remembers it', () => {
    applyTheme('dark');
    const { result } = renderHook(() => useTheme());
    expect(result.current[0]).toBe('dark');

    act(() => result.current[1]());
    expect(result.current[0]).toBe('light');
    expect(document.documentElement.dataset.theme).toBe('light');
    expect(storedTheme()).toBe('light');

    act(() => result.current[1]());
    expect(result.current[0]).toBe('dark');
    expect(document.documentElement.dataset.theme).toBe('dark');
    expect(storedTheme()).toBe('dark');
  });

  it('keeps every instance in step, so the sidebar and Settings agree', () => {
    applyTheme('dark');
    const sidebar = renderHook(() => useTheme());
    const settings = renderHook(() => useThemeChoice());
    act(() => settings.result.current[1]('light'));
    expect(sidebar.result.current[0]).toBe('light');
    act(() => sidebar.result.current[1]());
    expect(settings.result.current[0]).toBe('dark');
  });

  it('follows the operating system when told to', () => {
    vi.stubGlobal(
      'matchMedia',
      (q: string) => ({ matches: q === '(prefers-color-scheme: light)', addEventListener: () => {} }) as unknown as MediaQueryList,
    );
    const { result } = renderHook(() => useThemeChoice());
    act(() => result.current[1]('system'));
    expect(result.current[0]).toBe('system');
    expect(document.documentElement.dataset.theme).toBe('light');
    expect(storedTheme()).toBe('light');
  });

  it('still switches when the choice cannot be saved', () => {
    applyTheme('dark');
    vi.spyOn(Storage.prototype, 'setItem').mockImplementation(() => {
      throw new Error('QuotaExceededError');
    });
    const { result } = renderHook(() => useTheme());
    act(() => result.current[1]());
    expect(result.current[0]).toBe('light');
    expect(document.documentElement.dataset.theme).toBe('light');
  });
});
