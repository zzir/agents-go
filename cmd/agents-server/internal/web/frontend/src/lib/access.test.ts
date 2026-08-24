import { describe, expect, it } from 'vitest';
import { canDeleteRow, canEditRow } from '@/lib/access';

describe('canEditRow', () => {
  it('lets the owner edit their private row', () => {
    expect(canEditRow(false, 'u1', { scope: 'private', owner_id: 'u1' })).toBe(true);
  });
  it('keeps members out of global and foreign rows', () => {
    expect(canEditRow(false, 'u1', { scope: 'global' })).toBe(false);
    expect(canEditRow(false, 'u1', { scope: 'private', owner_id: 'u2' })).toBe(false);
  });
  it('lets admins edit global rows but not foreign private ones', () => {
    expect(canEditRow(true, 'a1', { scope: 'global' })).toBe(true);
    expect(canEditRow(true, 'a1', { scope: 'private', owner_id: 'u2' })).toBe(false);
    expect(canEditRow(true, 'a1', { scope: 'private', owner_id: 'a1' })).toBe(true);
  });
  it('denies everything while the caller is unknown', () => {
    expect(canEditRow(false, undefined, { scope: 'private', owner_id: 'u1' })).toBe(false);
  });
});

describe('canDeleteRow', () => {
  it('is edit for members', () => {
    expect(canDeleteRow(false, 'u1', { scope: 'private', owner_id: 'u1' })).toBe(true);
    expect(canDeleteRow(false, 'u1', { scope: 'global' })).toBe(false);
  });
  it('lets admins delete any row, foreign private included', () => {
    expect(canDeleteRow(true, 'a1', { scope: 'private', owner_id: 'u2' })).toBe(true);
    expect(canDeleteRow(true, 'a1', { scope: 'global' })).toBe(true);
  });
});
