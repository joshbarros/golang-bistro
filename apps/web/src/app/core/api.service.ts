import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Order, LogEntry, Sitting } from './models';

export interface NewSitting {
  code: string;
  name: string;
  room: string;
  chef: string;
  coversTotal: number;
  maxPerGuest: number;
  priceCents: number;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  sittings = () => firstValueFrom(this.http.get<Sitting[]>('/api/sittings'));
  openSitting = (body: NewSitting) =>
    firstValueFrom(this.http.post<Sitting>('/api/sittings', body));

  orders = (sittingCode: string) =>
    firstValueFrom(this.http.get<Order[]>('/api/orders', { params: { sittingCode } }));
  book = (body: { sittingCode: string; reference: string; covers: number; waitlist: boolean }) =>
    firstValueFrom(this.http.post<Order>('/api/orders', body));
  refund = (id: string) => firstValueFrom(this.http.post<Order>(`/api/orders/${id}/refund`, {}));
  events = (id: string) => firstValueFrom(this.http.get<LogEntry[]>(`/api/orders/${id}/events`));
}
