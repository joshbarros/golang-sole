import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Order, LogEntry, Raffle } from './models';

export interface NewRaffle {
  code: string;
  name: string;
  colorway: string;
  brand: string;
  pairsTotal: number;
  drawAt: string;
  priceCents: number;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  raffles = () => firstValueFrom(this.http.get<Raffle[]>('/api/raffles'));
  openRaffle = (body: NewRaffle) => firstValueFrom(this.http.post<Raffle>('/api/raffles', body));

  orders = (raffleCode: string) =>
    firstValueFrom(this.http.get<Order[]>('/api/orders', { params: { raffleCode } }));
  book = (body: { raffleCode: string; reference: string; pairs: number }) =>
    firstValueFrom(this.http.post<Order>('/api/orders', body));
  refund = (id: string) => firstValueFrom(this.http.post<Order>(`/api/orders/${id}/refund`, {}));
  events = (id: string) => firstValueFrom(this.http.get<LogEntry[]>(`/api/orders/${id}/events`));
}
