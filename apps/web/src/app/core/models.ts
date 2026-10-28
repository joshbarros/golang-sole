export interface Raffle {
  id: string;
  code: string;
  name: string;
  colorway: string;
  brand: string;
  drawAt: string;
  priceCents: number;
  pairsTotal: number;
  pairsLeft: number;
  status: string;
  seedHash: string;
  seed: string | null;
  drawnAt: string | null;
  entries: number;
}

export type OrderStatus = 'Requested' | 'Entered' | 'Held' | 'Sold' | 'Refunded' | 'Rejected';

export interface Order {
  id: string;
  raffleCode: string;
  ownerId: string;
  reference: string;
  pairs: number;
  totalCents: number;
  status: OrderStatus;
  reason: string | null;
  claimNumber: string | null;
  createdAt: string;
  updatedAt: string;
  pairsReleasedAt: string | null;
}

export interface LogEntry {
  id: string;
  type: string;
  module: string;
  occurredAt: string;
}

export interface UserInfo {
  id: string;
  email: string;
  roles: string[];
}

export interface AuthResponse {
  accessToken: string;
  expiresAt: string;
  user: UserInfo;
}
