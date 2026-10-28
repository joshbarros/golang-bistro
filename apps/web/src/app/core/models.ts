export interface Sitting {
  id: string;
  code: string;
  name: string;
  room: string;
  chef: string;
  serviceDate: string;
  priceCents: number;
  coversTotal: number;
  coversLeft: number;
  maxPerGuest: number;
}

export type OrderStatus = 'Requested' | 'Waitlisted' | 'Held' | 'Sold' | 'Refunded' | 'Rejected';

export interface Order {
  id: string;
  sittingCode: string;
  ownerId: string;
  reference: string;
  covers: number;
  totalCents: number;
  status: OrderStatus;
  reason: string | null;
  confirmationNumber: string | null;
  createdAt: string;
  updatedAt: string;
  coversReleasedAt: string | null;
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
