import { describe, expect, it } from 'vitest';
import { canDeleteRow, canDemoteRow, canEditRow, canReference } from '@/lib/access';

describe('canEditRow', () => {
  it('lets the author edit what they created, private or published', () => {
    expect(canEditRow(false, 'u1', { scope: 'private', owner_id: 'u1' })).toBe(true);
    expect(canEditRow(false, 'u1', { scope: 'global', owner_id: 'u1' })).toBe(true);
  });
  it('keeps members out of other people’s rows', () => {
    expect(canEditRow(false, 'u1', { scope: 'global', owner_id: 'u2' })).toBe(false);
    expect(canEditRow(false, 'u1', { scope: 'private', owner_id: 'u2' })).toBe(false);
  });
  it('lets admins edit global rows but not foreign private ones', () => {
    expect(canEditRow(true, 'a1', { scope: 'global', owner_id: 'u2' })).toBe(true);
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
    expect(canDeleteRow(false, 'u1', { scope: 'global', owner_id: 'u2' })).toBe(false);
  });
  it('lets admins delete any row, foreign private included', () => {
    expect(canDeleteRow(true, 'a1', { scope: 'private', owner_id: 'u2' })).toBe(true);
    expect(canDeleteRow(true, 'a1', { scope: 'global', owner_id: 'u2' })).toBe(true);
  });
});

describe('canDemoteRow', () => {
  it('is the admin’s or the author’s', () => {
    expect(canDemoteRow(false, 'u1', { scope: 'global', owner_id: 'u1' })).toBe(true);
    expect(canDemoteRow(false, 'u1', { scope: 'global', owner_id: 'u2' })).toBe(false);
    expect(canDemoteRow(true, 'a1', { scope: 'global', owner_id: 'u2' })).toBe(true);
  });
});

describe('canReference', () => {
  const global = { scope: 'global', owner_id: 'u1' };
  const mine = { scope: 'private', owner_id: 'u1' };
  const theirs = { scope: 'private', owner_id: 'u2' };

  it('lets a private holder name global rows and its owner’s own', () => {
    expect(canReference(mine, global)).toBe(true);
    expect(canReference(mine, mine)).toBe(true);
    expect(canReference(mine, theirs)).toBe(false);
  });
  it('lets a global holder name global rows only — its author’s included', () => {
    expect(canReference(global, global)).toBe(true);
    expect(canReference(global, mine)).toBe(false);
  });
});
