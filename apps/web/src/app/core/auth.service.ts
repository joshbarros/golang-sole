import { HttpClient } from '@angular/common/http';
import { Injectable, computed, inject, signal } from '@angular/core';
import { Observable, firstValueFrom } from 'rxjs';
import { AuthResponse, UserInfo } from './models';

/** Sent on refresh and logout so a page on another site cannot make the browser call them. */
export const REQUESTED_WITH = { 'X-Requested-With': 'sole-web' };

/**
 * The access token lives in memory only. The refresh token is an HttpOnly cookie the page cannot read,
 * so a reload signs the user back in through POST /api/auth/refresh and nothing sensitive is in storage.
 */
@Injectable({ providedIn: 'root' })
export class AuthService {
  private readonly http = inject(HttpClient);
  private readonly state = signal<{ token: string; user: UserInfo } | null>(null);
  private inFlight: Promise<boolean> | null = null;

  readonly user = computed(() => this.state()?.user ?? null);
  readonly isStaff = computed(() => {
    const roles = this.user()?.roles ?? [];
    return roles.includes('Admin') || roles.includes('Staff');
  });
  readonly ready = signal(false);

  token = () => this.state()?.token ?? null;

  /** Called once at startup: resumes the signed-in state when the refresh cookie is still good. */
  async restore(): Promise<void> {
    await this.refresh();
    this.ready.set(true);
  }

  login = (email: string, password: string, code?: string) =>
    this.enter(this.http.post<AuthResponse>('/api/auth/login', { email, password, code }));

  /** Returns a message when the address has to be confirmed first, or null when the user is already signed in. */
  async register(email: string, password: string): Promise<string | null> {
    const body = await firstValueFrom(
      this.http.post<AuthResponse | { message: string }>('/api/auth/register', { email, password }),
    );
    if ('accessToken' in body) {
      this.state.set({ token: body.accessToken, user: body.user });
      return null;
    }
    return body.message;
  }

  confirmEmail = (userId: string, token: string) =>
    firstValueFrom(this.http.post('/api/auth/confirm-email', { userId, token }));

  resendConfirmation = (email: string) =>
    firstValueFrom(this.http.post('/api/auth/resend-confirmation', { email }));

  forgotPassword = (email: string) =>
    firstValueFrom(this.http.post('/api/auth/forgot-password', { email }));

  resetPassword = (userId: string, token: string, newPassword: string) =>
    firstValueFrom(this.http.post('/api/auth/reset-password', { userId, token, newPassword }));

  changePassword = (currentPassword: string, newPassword: string) =>
    this.enter(
      this.http.post<AuthResponse>('/api/auth/change-password', { currentPassword, newPassword }),
    );

  async logoutEverywhere(): Promise<void> {
    try {
      await firstValueFrom(this.http.post('/api/auth/logout-everywhere', {}));
    } finally {
      this.state.set(null);
    }
  }

  mfaStatus = () => firstValueFrom(this.http.get<{ enabled: boolean }>('/api/auth/mfa'));
  mfaSetup = () =>
    firstValueFrom(
      this.http.post<{ sharedKey: string; otpAuthUri: string }>('/api/auth/mfa/setup', {}),
    );
  mfaEnable = (code: string) =>
    firstValueFrom(this.http.post<{ codes: string[] }>('/api/auth/mfa/enable', { code }));
  async mfaDisable(password: string, code: string): Promise<void> {
    await firstValueFrom(this.http.post('/api/auth/mfa/disable', { password, code }));
    this.state.set(null); // disabling ends every session; sign in again
  }

  /** Concurrent callers share one request: refresh tokens are single-use, so a second call would look like a replay. */
  refresh(): Promise<boolean> {
    this.inFlight ??= this.doRefresh().finally(() => (this.inFlight = null));
    return this.inFlight;
  }

  async logout(): Promise<void> {
    try {
      await firstValueFrom(this.http.post('/api/auth/logout', {}, { headers: REQUESTED_WITH }));
    } finally {
      this.state.set(null);
    }
  }

  private async doRefresh(): Promise<boolean> {
    try {
      const auth = await firstValueFrom(
        this.http.post<AuthResponse>('/api/auth/refresh', {}, { headers: REQUESTED_WITH }),
      );
      this.state.set({ token: auth.accessToken, user: auth.user });
      return true;
    } catch {
      this.state.set(null);
      return false;
    }
  }

  private async enter(request: Observable<AuthResponse>): Promise<void> {
    const auth = await firstValueFrom(request);
    this.state.set({ token: auth.accessToken, user: auth.user });
  }
}
