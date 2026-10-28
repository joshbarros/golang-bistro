import { Component, DestroyRef, OnInit, computed, inject, signal } from '@angular/core';
import { DatePipe } from '@angular/common';
import { ApiService } from './core/api.service';
import { AuthService } from './core/auth.service';
import { Order, OrderStatus, LogEntry, Sitting } from './core/models';

const BADGES: Record<OrderStatus, string> = {
  Requested: 'bg-slate-100 text-slate-700 ring-slate-300',
  Waitlisted: 'bg-violet-50 text-violet-800 ring-violet-300',
  Held: 'bg-amber-50 text-amber-800 ring-amber-300',
  Sold: 'bg-emerald-50 text-emerald-800 ring-emerald-300',
  Refunded: 'bg-sky-50 text-sky-800 ring-sky-300',
  Rejected: 'bg-rose-50 text-rose-800 ring-rose-300',
};

@Component({
  selector: 'app-workspace',
  imports: [DatePipe],
  templateUrl: './workspace.component.html',
})
export class WorkspaceComponent implements OnInit {
  private readonly api = inject(ApiService);
  readonly auth = inject(AuthService);
  private readonly destroyRef = inject(DestroyRef);

  readonly sittings = signal<Sitting[]>([]);
  readonly selectedCode = signal('');
  readonly orders = signal<Order[]>([]);
  readonly log = signal<LogEntry[]>([]);
  readonly logFor = signal<string | null>(null);
  readonly error = signal('');

  readonly code = signal('');
  readonly name = signal('Night One');
  readonly room = signal('Terrace');
  readonly chef = signal('Chez Marlo');
  readonly price = signal('5000');
  readonly capacity = signal('12');
  readonly limit = signal('8');

  readonly reference = signal('');
  readonly covers = signal('1');
  readonly waitlist = signal(false);

  readonly selected = computed(() => this.sittings().find((s) => s.code === this.selectedCode()));

  ngOnInit(): void {
    void this.refresh();
    const timer = setInterval(() => void this.refresh(), 1000);
    this.destroyRef.onDestroy(() => clearInterval(timer));
  }

  badge(status: OrderStatus): string {
    return BADGES[status];
  }

  money(cents: number): string {
    return (cents / 100).toLocaleString('en-US', { style: 'currency', currency: 'USD' });
  }

  async refresh(): Promise<void> {
    try {
      const sittings = await this.api.sittings();
      this.sittings.set(sittings);
      if (!this.selectedCode() && sittings.length > 0) this.selectedCode.set(sittings[0].code);
      if (this.selectedCode()) this.orders.set(await this.api.orders(this.selectedCode()));
      const id = this.logFor();
      if (id && this.auth.isStaff()) this.log.set(await this.api.events(id));
    } catch (e: unknown) {
      // A 401 means the session is gone; the interceptor has already signed the user out.
      if ((e as { status?: number }).status !== 401) this.error.set('The API is not reachable.');
      return;
    }
    if (this.error() === 'The API is not reachable.') this.error.set('');
  }

  async select(code: string): Promise<void> {
    this.selectedCode.set(code);
    this.logFor.set(null);
    this.log.set([]);
    await this.refresh();
  }

  async openSitting(): Promise<void> {
    await this.run(async () => {
      const code = this.code();
      const sitting = await this.api.openSitting({
        code,
        name: this.name(),
        room: this.room(),
        chef: this.chef(),
        coversTotal: Number(this.capacity()),
        maxPerGuest: Number(this.limit()),
        priceCents: Number(this.price()),
      });
      if (this.code() === code) this.code.set('');
      this.selectedCode.set(sitting.code);
    });
  }

  async book(): Promise<void> {
    await this.run(async () => {
      const reference = this.reference();
      await this.api.book({
        sittingCode: this.selectedCode(),
        reference,
        covers: Number(this.covers()),
        waitlist: this.waitlist(),
      });
      if (this.reference() === reference) this.reference.set('');
    });
  }

  toggleWaitlist(event: Event): void {
    this.waitlist.set((event.target as HTMLInputElement).checked);
  }

  async refund(order: Order): Promise<void> {
    await this.run(() => this.api.refund(order.id));
  }

  async showLog(order: Order): Promise<void> {
    this.logFor.set(order.id);
    this.log.set([]);
    await this.refresh();
  }

  canRefund(order: Order): boolean {
    return order.status === 'Held' || order.status === 'Sold' || order.status === 'Waitlisted';
  }

  private async run(action: () => Promise<unknown>): Promise<void> {
    this.error.set('');
    try {
      await action();
    } catch (e: unknown) {
      const body = (e as { error?: { title?: string } }).error;
      this.error.set(body?.title ?? 'The request failed.');
    }
    await this.refresh();
  }

  field(signalRef: { set(value: string): void }, event: Event): void {
    signalRef.set((event.target as HTMLInputElement).value);
  }
}
