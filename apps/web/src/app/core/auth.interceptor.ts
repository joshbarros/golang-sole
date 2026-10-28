import { HttpErrorResponse, HttpInterceptorFn, HttpRequest } from '@angular/common/http';
import { inject } from '@angular/core';
import { catchError, from, switchMap, throwError } from 'rxjs';
import { AuthService } from './auth.service';

/** Endpoints that are called while signed out: no token to attach, nothing to refresh. */
const ANONYMOUS = [
  'login',
  'register',
  'refresh',
  'logout',
  'confirm-email',
  'resend-confirmation',
  'forgot-password',
  'reset-password',
];
const isAuthCall = (req: HttpRequest<unknown>) =>
  ANONYMOUS.some((a) => req.url === `/api/auth/${a}`);

const withToken = (req: HttpRequest<unknown>, token: string | null) =>
  token ? req.clone({ setHeaders: { Authorization: `Bearer ${token}` } }) : req;

/** Adds the bearer token; on a 401 it refreshes once and replays the request, otherwise signs the user out. */
export const authInterceptor: HttpInterceptorFn = (req, next) => {
  const auth = inject(AuthService);
  if (isAuthCall(req)) return next(req);

  return next(withToken(req, auth.token())).pipe(
    catchError((error: unknown) => {
      if (!(error instanceof HttpErrorResponse) || error.status !== 401)
        return throwError(() => error);
      return from(auth.refresh()).pipe(
        switchMap((ok) => {
          if (!ok) return throwError(() => error);
          return next(withToken(req, auth.token()));
        }),
      );
    }),
  );
};
